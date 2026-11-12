package main

import (
	"encoding/json"
	"testing"
)

// CPA v8 normalizes an OpenAI Responses request into a chat-completions
// ExecutorRequest payload. In that normalization, reasoning.effort may be
// absent from Payload even though OriginalRequest still contains it. The
// WorkBuddy chat upstream only reads top-level reasoning_effort.
func TestPrepareUpstreamBody_RestoresResponsesReasoningEffortFromOriginal(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	original := []byte(`{"model":"deepseek-v4.1-flash","input":[{"role":"developer","content":"be brief"},{"role":"user","content":"hi"}],"reasoning":{"effort":"high","summary":"auto"},"stream":true}`)

	out := prepareUpstreamBody(payload, original, nil, "deepseek-v4.1-flash")
	obj := decodeBody(t, out)
	if got := obj["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got)
	}
	if got := obj["reasoning_summary"]; got != "auto" {
		t.Fatalf("reasoning_summary = %v, want auto", got)
	}
	if _, exists := obj["reasoning"]; exists {
		t.Fatalf("nested Responses reasoning must not be sent to chat upstream: %#v", obj["reasoning"])
	}
}

func TestPrepareUpstreamBody_RestoresChatReasoningEffortFromOriginal(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}]}`)
	original := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}],"reasoning_effort":"xhigh"}`)

	out := prepareUpstreamBody(payload, original, nil, "deepseek-v4.1-flash")
	obj := decodeBody(t, out)
	if got := obj["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	if got := obj["reasoning_summary"]; got != "auto" {
		t.Fatalf("reasoning_summary = %v, want auto fallback", got)
	}
}

func TestPrepareUpstreamBody_PayloadReasoningEffortWins(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}],"reasoning_effort":"low","reasoning_summary":"concise"}`)
	original := []byte(`{"model":"deepseek-v4.1-flash","reasoning":{"effort":"high","summary":"auto"}}`)

	out := prepareUpstreamBody(payload, original, nil, "deepseek-v4.1-flash")
	obj := decodeBody(t, out)
	if got := obj["reasoning_effort"]; got != "low" {
		t.Fatalf("payload effort must win, got %v", got)
	}
	if got := obj["reasoning_summary"]; got != "concise" {
		t.Fatalf("payload summary must win, got %v", got)
	}
}

func TestPrepareUpstreamBody_ConvertsNestedPayloadReasoning(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}],"reasoning":{"effort":"max","summary":"auto"}}`)

	out := prepareUpstreamBody(payload, nil, nil, "deepseek-v4.1-flash")
	obj := decodeBody(t, out)
	if got := obj["reasoning_effort"]; got != "max" {
		t.Fatalf("nested payload effort = %v, want max", got)
	}
	if _, exists := obj["reasoning"]; exists {
		t.Fatal("nested reasoning must be removed after conversion")
	}
}

func TestPrepareUpstreamBody_DoesNotInventEffort(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}]}`)
	original := []byte(`{"model":"deepseek-v4.1-flash","input":[{"role":"user","content":"hi"}]}`)

	out := prepareUpstreamBody(payload, original, nil, "deepseek-v4.1-flash")
	obj := decodeBody(t, out)
	if _, exists := obj["reasoning_effort"]; exists {
		t.Fatalf("must not invent a default effort: %#v", obj["reasoning_effort"])
	}
	if _, exists := obj["reasoning_summary"]; exists {
		t.Fatalf("must not invent summary without effort: %#v", obj["reasoning_summary"])
	}
}

func TestPrepareUpstreamBody_IgnoresMalformedOriginalReasoning(t *testing.T) {
	payload := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"be brief"}]}`)
	for _, original := range [][]byte{
		[]byte(`not-json`),
		[]byte(`{"reasoning":"high"}`),
		[]byte(`{"reasoning":{"effort":123}}`),
		[]byte(`{"reasoning_effort":""}`),
	} {
		out := prepareUpstreamBody(payload, original, nil, "deepseek-v4.1-flash")
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("output invalid JSON for original %q: %v", original, err)
		}
		if _, exists := obj["reasoning_effort"]; exists {
			t.Fatalf("malformed original %q injected effort %#v", original, obj["reasoning_effort"])
		}
	}
}

func TestNormalizeReasoningEffort_TrimsAndRejectsUnsafeTokens(t *testing.T) {
	for in, want := range map[string]string{
		" high ":                "high",
		"xhigh":                 "xhigh",
		"":                      "",
		"   ":                   "",
		"high\nX-Injected: yes": "",
		"this-token-is-far-too-long-to-be-a-reasoning-effort-value": "",
	} {
		if got := normalizeReasoningEffort(in); got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", in, got, want)
		}
	}
}
