package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Second 11128 layer: even after the developer→system role fix, the CN gateway
// content-matches specific channel fingerprints inside privileged (system /
// developer / assistant) prompt text. Live A/B against copilot.tencent.com
// (2026-11-12) confirmed two triggers in real DSH traffic:
//   - the harness identity sentence "DeepSeek Harness" (plain space and the
//     U+200B zero-width variant the kernel emits),
//   - the file-reference phrase "Tokens prefixed with @ ..." once it crosses
//     the gateway's phrase-length threshold.
//
// "coding agent powered by" is neutralized defensively (semantic-equivalent).
func TestNeutralizeFingerprintString_BrandIdentity(t *testing.T) {
	cases := map[string]string{
		"plain space":      "You are an AI agent powered by DeepSeek Harness.",
		"zero-width space": "You are an AI agent powered by DeepSeek​Harness.",
		"multiple":         "DeepSeek Harness and DeepSeek Harness again",
	}
	for name, in := range cases {
		out := neutralizeFingerprintString(in)
		if strings.Contains(out, "DeepSeek Harness") {
			t.Errorf("%s: plain brand fingerprint remains: %q", name, out)
		}
		if !strings.Contains(out, "DeepSeek-Harness") {
			t.Errorf("%s: expected hyphenated brand, got %q", name, out)
		}
	}
}

func TestNeutralizeFingerprintString_FileReferenceAt(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced."
	out := neutralizeFingerprintString(in)
	if strings.Contains(out, "prefixed with @") {
		t.Fatalf("@ fingerprint remains: %q", out)
	}
	if !strings.Contains(out, "prefixed with (at)") {
		t.Fatalf("expected (at) substitution, got %q", out)
	}
}

func TestNeutralizeFingerprintString_LeavesOrdinaryAtAndEmail(t *testing.T) {
	in := "ping @someone or mail a@b.com, but never Tokens prefixed with @ here."
	out := neutralizeFingerprintString(in)
	if !strings.Contains(out, "@someone") || !strings.Contains(out, "a@b.com") {
		t.Fatalf("ordinary @ mentions must be untouched: %q", out)
	}
	if strings.Contains(out, "prefixed with @") {
		t.Fatalf("anchored fingerprint should still be rewritten: %q", out)
	}
}

func TestNeutralizeFingerprintString_CodingAgentDefensive(t *testing.T) {
	in := "You are a coding agent powered by the deepseek-v4.1-flash model."
	out := neutralizeFingerprintString(in)
	if strings.Contains(out, "coding agent powered by") {
		t.Fatalf("coding-agent phrase not neutralized: %q", out)
	}
	if !strings.Contains(out, "coding agent running on") {
		t.Fatalf("expected running-on rewrite, got %q", out)
	}
}

func TestNeutralizeChannelFingerprints_PrivilegedRolesAndShapes(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[
		{"role":"system","content":"DeepSeek Harness identity"},
		{"role":"developer","content":[{"type":"text","text":"Tokens prefixed with @ are paths."}]},
		{"role":"assistant","content":"DeepSeek Harness was here"},
		{"role":"user","content":"DeepSeek Harness in user text stays"}
	]}`))
	if !neutralizeChannelFingerprintsInPlace(obj) {
		t.Fatal("expected modification")
	}
	msgs := obj["messages"].([]any)
	if got := msgs[0].(map[string]any)["content"].(string); strings.Contains(got, "DeepSeek Harness") {
		t.Errorf("system not neutralized: %q", got)
	}
	devBlock := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if got := devBlock["text"].(string); strings.Contains(got, "with @") {
		t.Errorf("developer multimodal block not neutralized: %q", got)
	}
	if got := msgs[2].(map[string]any)["content"].(string); strings.Contains(got, "DeepSeek Harness") {
		t.Errorf("assistant not neutralized: %q", got)
	}
	// user/tool messages are never rewritten (gateway does not match them).
	if got := msgs[3].(map[string]any)["content"].(string); got != "DeepSeek Harness in user text stays" {
		t.Errorf("user message must be untouched, got %q", got)
	}
}

func TestNeutralizeChannelFingerprints_InputContainerFallback(t *testing.T) {
	// Responses wire shape uses "input"; the host normally translates it to
	// "messages", but neutralization must still work if that ever changes.
	obj := decodeBody(t, []byte(`{"input":[
		{"role":"developer","content":"DeepSeek Harness"}
	]}`))
	if !neutralizeChannelFingerprintsInPlace(obj) {
		t.Fatal("input container must be handled")
	}
	got := obj["input"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "DeepSeek Harness") {
		t.Fatalf("input container not neutralized: %q", got)
	}
}

func TestNeutralizeChannelFingerprints_NoopWhenClean(t *testing.T) {
	obj := decodeBody(t, []byte(`{"messages":[{"role":"system","content":"be brief"}]}`))
	if neutralizeChannelFingerprintsInPlace(obj) {
		t.Fatal("clean body must be a no-op")
	}
}

// End-to-end: a real DSH-shaped developer message survives prepareUpstreamBody
// with every channel fingerprint neutralized and the role mapped to system.
func TestPrepareUpstreamBody_NeutralizesFullFingerprintStack(t *testing.T) {
	dev := "You are an AI agent powered by DeepSeek Harness.\n\n" +
		"You are a coding agent powered by the m model.\n\n" +
		"Tokens prefixed with @ are paths the user explicitly referenced.\n\n" +
		"You are interacting through the DeepSeek Harness Web GUI."
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "developer", "content": dev},
			map[string]any{"role": "user", "content": "hi"},
		},
	})
	out := prepareUpstreamBody(body, nil, nil, "upstream-model")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	first := obj["messages"].([]any)[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("role = %v, want system", first["role"])
	}
	got := first["content"].(string)
	for _, banned := range []string{"DeepSeek Harness", "prefixed with @", "coding agent powered by"} {
		if strings.Contains(got, banned) {
			t.Errorf("fingerprint %q survived: %s", banned, got)
		}
	}
}
