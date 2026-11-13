package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAlignWorkBuddyTurnOrderInPlaceRepairsToolUserTail(t *testing.T) {
	body := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "rules"},
		map[string]any{"role": "assistant", "tool_calls": []any{}},
		map[string]any{"role": "tool", "content": "result"},
		map[string]any{"role": "user", "content": "continue"},
		map[string]any{"role": "user", "content": "now"},
	}}
	if !alignWorkBuddyTurnOrderInPlace(body) {
		t.Fatal("expected alignment change")
	}
	messages := body["messages"].([]any)
	if len(messages) != 6 {
		t.Fatalf("message count = %d, want 6", len(messages))
	}
	inserted := messages[3].(map[string]any)
	if inserted["role"] != "assistant" || inserted["content"] != "" {
		t.Fatalf("inserted message = %#v", inserted)
	}
	if messages[4].(map[string]any)["content"] != "continue" || messages[5].(map[string]any)["content"] != "now" {
		t.Fatalf("user tail was reordered: %#v", messages)
	}
}

func TestAlignWorkBuddyTurnOrderLeavesOrdinaryTailUntouched(t *testing.T) {
	cases := []map[string]any{
		{"messages": []any{map[string]any{"role": "assistant"}, map[string]any{"role": "user"}}},
		{"messages": []any{map[string]any{"role": "tool"}, map[string]any{"role": "assistant"}}},
		{"messages": []any{map[string]any{"role": "user"}}},
	}
	for _, body := range cases {
		if alignWorkBuddyTurnOrderInPlace(body) {
			t.Fatalf("unexpected alignment for %#v", body)
		}
	}
}

func TestPrepareUpstreamRequestNormalizesMaxTokensAndCarriesToolMap(t *testing.T) {
	prepared := prepareUpstreamRequest([]byte(`{
		"model":"client-model",
		"max_completion_tokens":123,
		"messages":[{"role":"tool","name":"bad.name","content":"ok"},{"role":"user","content":"next"}],
		"tools":[{"type":"function","function":{"name":"bad.name","parameters":{"type":"object"}}}]
	}`), nil, nil, "upstream-model")
	var body map[string]any
	if err := json.Unmarshal(prepared.Body, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["max_completion_tokens"]; exists {
		t.Fatal("max_completion_tokens must be removed")
	}
	if body["max_tokens"] != float64(123) {
		t.Fatalf("max_tokens = %#v", body["max_tokens"])
	}
	safe := prepared.ToolNames.OriginalToSafe["bad.name"]
	if !validWorkBuddyToolName(safe) || !strings.HasPrefix(safe, "tool_") {
		t.Fatalf("safe name = %q", safe)
	}
	messages := body["messages"].([]any)
	if messages[2].(map[string]any)["role"] != "assistant" {
		t.Fatalf("turn boundary was not repaired: %#v", messages)
	}
	if messages[1].(map[string]any)["name"] != safe {
		t.Fatalf("tool history name was not rewritten: %#v", messages[1])
	}
}
