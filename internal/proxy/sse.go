package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxSSEEventBytes = 16 << 20

type sseEvent struct {
	Type string
	Data string
}

type streamEvent struct {
	Type     string          `json:"type"`
	Delta    string          `json:"delta,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
	Item     json.RawMessage `json:"item,omitempty"`
}

type streamState struct {
	fallbackModel string
	text          strings.Builder
	output        []json.RawMessage
	sawMessage    bool
	terminal      json.RawMessage
	terminalType  string
	streamError   bool
}

func RelaySSE(w http.ResponseWriter, body io.Reader, fallbackModel string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support streaming")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	state := &streamState{fallbackModel: fallbackModel}
	err := iterateSSE(body, func(event sseEvent) error {
		data := strings.TrimSpace(event.Data)
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			return writeSSEEvent(w, flusher, event.Type, data)
		}

		parsed, eventType, ok := parseStreamEvent(event)
		if ok {
			if err := state.remember(parsed, eventType); err != nil {
				return err
			}
			if eventType == "response.completed" || eventType == "response.incomplete" {
				patched, synthetic, err := state.patchTerminal(parsed.Response)
				if err != nil {
					return err
				}
				if len(synthetic) > 0 {
					frame := map[string]any{
						"type":         "response.output_item.done",
						"output_index": len(state.output) - 1,
						"item":         synthetic,
					}
					encoded, _ := json.Marshal(frame)
					if err := writeSSEEvent(w, flusher, "response.output_item.done", string(encoded)); err != nil {
						return err
					}
				}
				frame := map[string]any{"type": eventType, "response": patched}
				encoded, _ := json.Marshal(frame)
				return writeSSEEvent(w, flusher, eventType, string(encoded))
			}
		}
		if eventType == "" {
			eventType = event.Type
		}
		return writeSSEEvent(w, flusher, eventType, data)
	})
	if err != nil {
		frame, _ := json.Marshal(map[string]any{"type": "error", "message": "upstream stream ended unexpectedly"})
		_ = writeSSEEvent(w, flusher, "error", string(frame))
		return err
	}
	if state.terminalType == "" {
		frame, _ := json.Marshal(map[string]any{"type": "error", "message": "upstream stream ended without a terminal event"})
		_ = writeSSEEvent(w, flusher, "error", string(frame))
		return errors.New("upstream stream ended without a terminal event")
	}
	return nil
}

func BufferSSE(body io.Reader, fallbackModel string) ([]byte, error) {
	state := &streamState{fallbackModel: fallbackModel}
	err := iterateSSE(body, func(event sseEvent) error {
		data := strings.TrimSpace(event.Data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		parsed, eventType, ok := parseStreamEvent(event)
		if !ok {
			return errors.New("upstream sent a malformed SSE event")
		}
		return state.remember(parsed, eventType)
	})
	if err != nil {
		return nil, err
	}
	if state.terminalType == "" {
		return nil, errors.New("upstream stream ended without a terminal event")
	}
	if state.streamError {
		return nil, errors.New("upstream sent an error event")
	}
	if state.terminalType == "response.failed" {
		return nil, errors.New("upstream response failed")
	}
	if responseHasOutput(state.terminal) {
		return state.terminal, nil
	}
	if state.terminalType == "response.incomplete" && len(state.output) == 0 && state.text.Len() == 0 {
		return nil, errors.New("upstream response was incomplete and contained no output")
	}
	patched, _, err := state.patchTerminal(state.terminal)
	if err != nil {
		return nil, err
	}
	if !responseHasOutput(patched) {
		return nil, errors.New("upstream stream completed without output")
	}
	return patched, nil
}

func (s *streamState) remember(event streamEvent, eventType string) error {
	switch eventType {
	case "response.output_text.delta":
		s.text.WriteString(event.Delta)
	case "response.output_item.done":
		if len(event.Item) == 0 {
			return nil
		}
		if !json.Valid(event.Item) {
			return errors.New("upstream sent an invalid output item")
		}
		item := append(json.RawMessage(nil), event.Item...)
		s.output = append(s.output, item)
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item, &probe) == nil && probe.Type == "message" {
			s.sawMessage = true
		}
	case "response.completed", "response.incomplete", "response.failed":
		s.terminalType = eventType
		if len(event.Response) > 0 {
			if !json.Valid(event.Response) {
				return errors.New("upstream sent an invalid terminal response")
			}
			s.terminal = append(json.RawMessage(nil), event.Response...)
		}
	case "error":
		s.streamError = true
	}
	return nil
}

func (s *streamState) patchTerminal(raw json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	if responseHasOutput(raw) {
		return raw, nil, nil
	}
	output := append([]json.RawMessage(nil), s.output...)
	var synthetic json.RawMessage
	if s.text.Len() > 0 && !s.sawMessage {
		synthetic, _ = json.Marshal(map[string]any{
			"id":     fmt.Sprintf("msg_proxy_%d", time.Now().UnixNano()),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}},
			},
		})
		output = append(output, synthetic)
		s.output = output
		s.sawMessage = true
	}

	var response map[string]any
	if len(raw) == 0 {
		response = make(map[string]any)
	} else if err := json.Unmarshal(raw, &response); err != nil {
		return nil, nil, errors.New("upstream sent an invalid terminal response")
	}
	now := time.Now().Unix()
	setDefault(response, "id", fmt.Sprintf("resp_proxy_%d", now))
	setDefault(response, "object", "response")
	if _, ok := response["created_at"]; !ok {
		response["created_at"] = now
	}
	setDefault(response, "status", "completed")
	setDefault(response, "model", s.fallbackModel)
	if _, ok := response["usage"]; !ok || response["usage"] == nil {
		response["usage"] = map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	if _, ok := response["error"]; !ok {
		response["error"] = nil
	}
	response["output"] = output
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, nil, fmt.Errorf("encode terminal response: %w", err)
	}
	return encoded, synthetic, nil
}

func iterateSSE(reader io.Reader, yield func(sseEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSSEEventBytes)
	var eventType string
	var data []string

	dispatch := func() error {
		if len(data) == 0 {
			eventType = ""
			return nil
		}
		event := sseEvent{Type: eventType, Data: strings.Join(data, "\n")}
		eventType = ""
		data = data[:0]
		return yield(event)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			data = append(data, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read SSE stream: %w", err)
	}
	return dispatch()
}

func parseStreamEvent(event sseEvent) (streamEvent, string, bool) {
	var parsed streamEvent
	if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
		return streamEvent{}, event.Type, false
	}
	if parsed.Type != "" {
		return parsed, parsed.Type, true
	}
	return parsed, event.Type, true
}

func writeSSEEvent(writer io.Writer, flusher http.Flusher, eventType, data string) error {
	if eventType != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", eventType); err != nil {
			return err
		}
	}
	for _, line := range strings.Split(data, "\n") {
		if _, err := fmt.Fprintf(writer, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(writer, "\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func responseHasOutput(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var response struct {
		Output []json.RawMessage `json:"output"`
	}
	return json.Unmarshal(raw, &response) == nil && len(response.Output) > 0
}

func setDefault(object map[string]any, key string, value any) {
	current, ok := object[key]
	if !ok || current == nil || current == "" {
		object[key] = value
	}
}
