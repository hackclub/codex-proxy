package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBufferSSEUsesCompletedOutput(t *testing.T) {
	completed := `{"id":"resp_1","object":"response","status":"completed","model":"gpt-test","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":3},"output_tokens":8,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":28}}`
	stream := "event: response.completed\n" +
		`data: {"type":"response.completed","response":` + completed + "}\n\n"

	got, details, err := BufferSSE(strings.NewReader(stream), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != completed {
		t.Fatalf("completed response changed:\n%s", got)
	}
	if details.ID != "resp_1" || details.InputTokens != 20 || details.CachedTokens != 5 || details.CacheWriteTokens != 3 || details.OutputTokens != 8 || details.ReasoningTokens != 2 || details.TotalTokens != 28 {
		t.Fatalf("details = %#v", details)
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

	got, _, err := BufferSSE(strings.NewReader(stream), "fallback")
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
	if _, _, err := BufferSSE(strings.NewReader(stream), "gpt-test"); err == nil {
		t.Fatal("accepted a stream without a terminal event")
	}
}

func TestBufferSSERejectsMalformedEvent(t *testing.T) {
	if _, _, err := BufferSSE(strings.NewReader("data: not-json\n\n"), "gpt-test"); err == nil {
		t.Fatal("accepted malformed SSE data")
	}
}

func TestRelaySSEPatchesTerminalOutput(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-test","usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}}`,
		``,
	}, "\n")
	recorder := httptest.NewRecorder()
	details, err := RelaySSE(recorder, strings.NewReader(stream), "gpt-test")
	if err != nil {
		t.Fatal(err)
	}
	if details.InputTokens != 4 || details.OutputTokens != 3 || details.TotalTokens != 7 {
		t.Fatalf("details = %#v", details)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `event: response.output_item.done`) || !strings.Contains(body, `"output":[`) {
		t.Fatalf("relay did not reconstruct output:\n%s", body)
	}
}

func TestResponseDetailsFromCapturesToolUsage(t *testing.T) {
	details := responseDetailsFrom(json.RawMessage(`{
		"id":"resp_1",
		"model":"gpt-test",
		"status":"incomplete",
		"service_tier":"priority",
		"incomplete_details":{"reason":"max_output_tokens"},
		"tool_usage":{
			"web_search":{"num_requests":2},
			"image_gen":{
				"input_tokens":11,
				"input_tokens_details":{"image_tokens":7,"text_tokens":4},
				"output_tokens":13,
				"output_tokens_details":{"image_tokens":12,"text_tokens":1},
				"total_tokens":24
			}
		}
	}`))
	if details.ID != "resp_1" || details.Status != "incomplete" || details.ServiceTier != "priority" || details.WebSearchRequests != 2 {
		t.Fatalf("details = %#v", details)
	}
	if details.ImageGen.InputTokens != 11 || details.ImageGen.InputImageTokens != 7 || details.ImageGen.InputTextTokens != 4 || details.ImageGen.OutputTokens != 13 || details.ImageGen.OutputImageTokens != 12 || details.ImageGen.OutputTextTokens != 1 || details.ImageGen.TotalTokens != 24 {
		t.Fatalf("image usage = %#v", details.ImageGen)
	}
	if string(details.IncompleteDetails) != `{"reason":"max_output_tokens"}` {
		t.Fatalf("incomplete details = %s", details.IncompleteDetails)
	}
}
