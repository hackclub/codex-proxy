package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBufferSSEUsesCompletedOutput(t *testing.T) {
	completed := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`
	stream := "event: response.completed\n" +
		`data: {"type":"response.completed","response":` + completed + "}\n\n"

	got, err := BufferSSE(strings.NewReader(stream), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != completed {
		t.Fatalf("completed response changed:\n%s", got)
	}
}

func TestBufferSSEReconstructsOutput(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hel"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"lo"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"gpt-test"}}`,
		``,
	}, "\n")

	got, err := BufferSSE(strings.NewReader(stream), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(got, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 2 || response.Output[0]["type"] != "function_call" || response.Output[1]["type"] != "message" {
		t.Fatalf("unexpected output: %s", got)
	}
	content := response.Output[1]["content"].([]any)[0].(map[string]any)
	if content["text"] != "hello" {
		t.Fatalf("text = %#v", content["text"])
	}
}

func TestBufferSSERequiresTerminalEvent(t *testing.T) {
	stream := "event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n"
	if _, err := BufferSSE(strings.NewReader(stream), "gpt-test"); err == nil {
		t.Fatal("accepted a stream without a terminal event")
	}
}

func TestBufferSSERejectsMalformedEvent(t *testing.T) {
	if _, err := BufferSSE(strings.NewReader("data: not-json\n\n"), "gpt-test"); err == nil {
		t.Fatal("accepted malformed SSE data")
	}
}

func TestRelaySSEPatchesTerminalOutput(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-test"}}`,
		``,
	}, "\n")
	recorder := httptest.NewRecorder()
	if err := RelaySSE(recorder, strings.NewReader(stream), "gpt-test"); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_item.done`) || !strings.Contains(body, `"output":[`) {
		t.Fatalf("relay did not reconstruct output:\n%s", body)
	}
}
