package proxy

import (
	"encoding/json"
	"testing"
)

func TestPatchPayload(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-test",
		"input":"hello",
		"stream":false,
		"store":true,
		"max_output_tokens":100,
		"max_completion_tokens":200,
		"tools":[{"type":"web_search_preview"},{"type":"function","name":"lookup"}]
	}`)

	patched, metadata, err := PatchPayload(raw, "default instructions")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Model != "gpt-test" || metadata.DownstreamStream {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}

	var payload map[string]any
	if err := json.Unmarshal(patched, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true || payload["store"] != false {
		t.Fatalf("stream/store were not patched: %s", patched)
	}
	if payload["instructions"] != "default instructions" {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
	if _, exists := payload["max_output_tokens"]; exists {
		t.Fatal("max_output_tokens survived")
	}
	if _, exists := payload["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens survived")
	}
	include := payload["include"].([]any)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", include)
	}
	input := payload["input"].([]any)
	if input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("string input was not normalized: %#v", input)
	}
	tools := payload["tools"].([]any)
	if tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tool was not renamed: %#v", tools)
	}
}

func TestPatchPayloadForResponsesLite(t *testing.T) {
	patched, metadata, err := PatchPayload([]byte(`{
		"model":"gpt-5.6-terra",
		"instructions":"be terse",
		"input":"hello",
		"tools":[{"type":"function","name":"lookup"}],
		"parallel_tool_calls":true
	}`), "default")
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.ResponsesLite {
		t.Fatal("gpt-5.6-terra was not recognized as Responses Lite")
	}

	var payload map[string]any
	if err := json.Unmarshal(patched, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload["instructions"]; ok {
		t.Fatal("top-level instructions survived")
	}
	if _, ok := payload["tools"]; ok {
		t.Fatal("top-level tools survived")
	}
	if payload["parallel_tool_calls"] != false || payload["tool_choice"] != "auto" {
		t.Fatalf("tool settings = %#v, %#v", payload["parallel_tool_calls"], payload["tool_choice"])
	}
	input := payload["input"].([]any)
	if len(input) != 3 || input[0].(map[string]any)["type"] != "additional_tools" || input[1].(map[string]any)["role"] != "developer" || input[2].(map[string]any)["role"] != "user" {
		t.Fatalf("unexpected input: %#v", input)
	}
	tools := input[0].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("unexpected tools: %#v", tools)
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["context"] != "all_turns" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestPatchPayloadForResponsesLiteWithoutTools(t *testing.T) {
	patched, _, err := PatchPayload([]byte(`{"model":"gpt-5.6-luna","input":"hello"}`), "default")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(patched, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["input"].([]any)[0].(map[string]any)["tools"]
	if tools == nil || len(tools.([]any)) != 0 {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestPatchPayloadPreservesInstructionsAndDropsReasoning(t *testing.T) {
	raw := []byte(`{
		"instructions":"be terse",
		"stream":true,
		"input":[
			{"type":"reasoning","id":"rs_missing"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)
	patched, metadata, err := PatchPayload(raw, "default")
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.DownstreamStream {
		t.Fatal("lost downstream stream preference")
	}

	var payload map[string]any
	_ = json.Unmarshal(patched, &payload)
	if payload["instructions"] != "be terse" {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
	input := payload["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("unexpected input: %#v", input)
	}
}

func TestPatchPayloadRejectsInvalidJSON(t *testing.T) {
	if _, _, err := PatchPayload([]byte(`[]`), "default"); err == nil {
		t.Fatal("accepted a non-object body")
	}
}
