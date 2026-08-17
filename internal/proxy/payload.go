package proxy

import (
	"encoding/json"
	"errors"
	"strings"
)

type RequestMetadata struct {
	Model            string
	DownstreamStream bool
	ResponsesLite    bool
}

func PatchPayload(raw []byte, defaultInstructions string) ([]byte, RequestMetadata, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, RequestMetadata{}, errors.New("request body must be a JSON object")
	}
	if payload == nil {
		return nil, RequestMetadata{}, errors.New("request body must be a JSON object")
	}

	metadata := RequestMetadata{}
	metadata.Model, _ = payload["model"].(string)
	metadata.DownstreamStream, _ = payload["stream"].(bool)
	metadata.ResponsesLite = strings.HasPrefix(metadata.Model, "gpt-5.6-") || metadata.Model == "codex-auto-review"

	payload["stream"] = true
	payload["store"] = false
	delete(payload, "max_output_tokens")
	delete(payload, "max_completion_tokens")

	if instructions, ok := payload["instructions"].(string); !ok || instructions == "" {
		payload["instructions"] = defaultInstructions
	}
	normalizeStringInput(payload)
	stripReasoningItems(payload)
	renameTools(payload)
	includeEncryptedReasoning(payload)
	if metadata.ResponsesLite {
		applyResponsesLite(payload)
	}

	patched, err := json.Marshal(payload)
	if err != nil {
		return nil, RequestMetadata{}, errors.New("could not encode patched request")
	}
	return patched, metadata, nil
}

func includeEncryptedReasoning(payload map[string]any) {
	include, _ := payload["include"].([]any)
	for _, item := range include {
		if item == "reasoning.encrypted_content" {
			return
		}
	}
	payload["include"] = append(include, "reasoning.encrypted_content")
}

func applyResponsesLite(payload map[string]any) {
	input, _ := payload["input"].([]any)
	tools := []any{}
	if declared, ok := payload["tools"].([]any); ok {
		tools = declared
	}
	prefix := []any{map[string]any{
		"type":  "additional_tools",
		"role":  "developer",
		"tools": tools,
	}}
	if instructions, _ := payload["instructions"].(string); instructions != "" {
		prefix = append(prefix, map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{"type": "input_text", "text": instructions},
			},
		})
	}
	payload["input"] = append(prefix, input...)
	if _, ok := payload["parallel_tool_calls"]; !ok {
		payload["parallel_tool_calls"] = false
	}
	if payload["tool_choice"] != "none" && payload["tool_choice"] != "required" {
		payload["tool_choice"] = "auto"
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	reasoning["context"] = "all_turns"
	payload["reasoning"] = reasoning
	delete(payload, "instructions")
	delete(payload, "tools")
}

func normalizeStringInput(payload map[string]any) {
	input, ok := payload["input"].(string)
	if !ok {
		return
	}
	payload["input"] = []any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": input},
			},
		},
	}
}

func stripReasoningItems(payload map[string]any) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(input))
	for _, item := range input {
		object, ok := item.(map[string]any)
		if ok && object["type"] == "reasoning" {
			continue
		}
		filtered = append(filtered, item)
	}
	payload["input"] = filtered
}

func renameTools(payload map[string]any) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] == "web_search_preview" {
			tool["type"] = "web_search"
		}
	}
}
