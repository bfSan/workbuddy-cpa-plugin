package main

import (
	"encoding/json"
	"testing"
)

// Regression: host v8.0.61+ keeps the OpenAI Responses "developer" role
// instead of downgrading it to user. The WorkBuddy chat upstream only accepts
// system as the privileged prompt role and requires the first message to be a
// system message; otherwise the CN gateway returns 400 code 11128
// "Illegal API invocation from an unapproved channel".
func TestNormalizeSystemFirst_DeveloperMappedToSystemInPlace(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"developer","content":"You are a helpful assistant."},
		{"role":"user","content":"hi"}
	]}`))

	if !normalizeSystemFirstInPlace(obj) {
		t.Fatal("expected modification when a developer role is present")
	}
	messages := obj["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role = %v, want system", first["role"])
	}
	if first["content"] != "You are a helpful assistant." {
		t.Fatalf("content must be preserved in place, got %v", first["content"])
	}
	if messages[1].(map[string]any)["role"] != "user" {
		t.Fatal("non-developer roles must not be touched")
	}
}

func TestNormalizeSystemFirst_DeveloperRoleCaseInsensitive(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[{"role":"Developer","content":"x"}]}`))
	if !normalizeSystemFirstInPlace(obj) {
		t.Fatal("Developer (capitalized) must be recognized")
	}
	if got := obj["messages"].([]any)[0].(map[string]any)["role"]; got != "system" {
		t.Fatalf("role = %v, want system", got)
	}
}

func TestNormalizeSystemFirst_DeveloperMultimodalContentPreserved(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"developer","content":[{"type":"text","text":"be brief"}]},
		{"role":"user","content":"hi"}
	]}`))
	normalizeSystemFirstInPlace(obj)
	first := obj["messages"].([]any)[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("role = %v, want system", first["role"])
	}
	parts := first["content"].([]any)
	if parts[0].(map[string]any)["text"] != "be brief" {
		t.Fatalf("multimodal content mutated: %#v", first["content"])
	}
}

func TestNormalizeSystemFirst_InjectsSystemWhenFirstMessageIsNotSystem(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"}
	]}`))
	if !normalizeSystemFirstInPlace(obj) {
		t.Fatal("expected a placeholder system message to be injected")
	}
	messages := obj["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("len(messages) = %d, want 3", len(messages))
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first role = %v, want system", first["role"])
	}
	if c, _ := first["content"].(string); c == "" {
		t.Fatal("placeholder system message needs non-empty content")
	}
}

func TestNormalizeSystemFirst_NoInjectionWhenSystemAlreadyFirst(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"hi"}
	]}`))
	if normalizeSystemFirstInPlace(obj) {
		t.Fatal("must be a no-op when a system message already leads")
	}
	if len(obj["messages"].([]any)) != 2 {
		t.Fatal("message count must not change")
	}
}

func TestNormalizeSystemFirst_DeveloperAfterSystemStillMapped(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"system","content":"sys"},
		{"role":"developer","content":"extra instructions"},
		{"role":"user","content":"hi"}
	]}`))
	if !normalizeSystemFirstInPlace(obj) {
		t.Fatal("developer role after a system message must still be normalized")
	}
	messages := obj["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatal("leading system must stay first")
	}
	if messages[1].(map[string]any)["role"] != "system" {
		t.Fatalf("second role = %v, want system", messages[1].(map[string]any)["role"])
	}
	if len(messages) != 3 {
		t.Fatal("no placeholder should be injected when system already leads")
	}
}

func TestNormalizeSystemFirst_EmptyAndMalformedBodies(t *testing.T) {
	if normalizeSystemFirstInPlace(map[string]any{}) {
		t.Fatal("missing messages must be a no-op")
	}
	obj := decodeBody(t, []byte(`{"messages":[]}`))
	if normalizeSystemFirstInPlace(obj) {
		t.Fatal("empty messages must be a no-op")
	}
	// Non-map elements must not panic; map ones still get normalized. A leading
	// non-map element is not a system message, so a placeholder gets prepended.
	obj2 := map[string]any{"messages": []any{"junk", map[string]any{"role": "developer", "content": "x"}}}
	if !normalizeSystemFirstInPlace(obj2) {
		t.Fatal("valid developer message among junk must still be normalized")
	}
	messages := obj2["messages"].([]any)
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("placeholder first role = %v, want system", messages[0])
	}
	// Original order preserved after the placeholder; developer mapped to system.
	dev := messages[2].(map[string]any)
	if dev["role"] != "system" || dev["content"] != "x" {
		t.Fatalf("developer message = %#v, want role system with content preserved", dev)
	}
}

// End-to-end through the single compose point used by both execute paths.
func TestPrepareUpstreamBody_NormalizesDeveloperRole(t *testing.T) {
	body := []byte(`{"model":"workbuddy/fast","messages":[
		{"role":"developer","content":"You are a helpful assistant."},
		{"role":"user","content":"hi"}
	]}`)
	out := prepareUpstreamBody(body, nil, nil, "upstream-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	messages := obj["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("after prepareUpstreamBody first role = %v, want system (11128 guard)", first["role"])
	}
}
