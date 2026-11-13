package main

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestBackendHeadersAddsClientProfileWithoutChatIdentity(t *testing.T) {
	req := httptest.NewRequest("GET", "https://example.com/v3/config", nil)
	backendHeaders(req, &storedAuth{})
	for name, want := range map[string]string{
		"X-IDE-Type":    "CLI",
		"X-IDE-Name":    "CLI",
		"X-IDE-Version": codeBuddyCLIVersion,
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"X-Request-ID", "X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "acp-connection-id", "X-Agent-Intent", "X-Agent-Purpose", "X-CodeBuddy-Request", "traceparent"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("non-chat %s = %q, want empty", name, got)
		}
	}
}

func TestBackendHeadersStripUntrustedProviderAndSessionHeaders(t *testing.T) {
	req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
	for key, value := range map[string]string{
		"Authorization":            "Bearer attacker",
		"X-Refresh-Token":          "secret-refresh",
		"X-DSH-Session-ID":         "raw-dsh-session",
		"X-Session-ID":             "raw-session",
		"Session-ID":               "raw-session-2",
		"OpenAI-Conversation-ID":   "raw-openai-session",
		"X-Conversation-ID":        "old-conversation",
		"X-Agent-Type":             "spoofed",
		"X-IDE-Version":            "0.0.0",
		"traceparent":              "spoofed-trace",
		"Cookie":                   "secret=cookie",
		"Forwarded":                "for=192.0.2.1",
		"X-Client-Platform":        "spoofed-client",
		"X-OpenAI-Conversation-ID": "raw-openai-alt",
		"X-Amzn-Trace-Id":          "raw-amzn-trace",
		"Sentry-Trace":             "raw-sentry-trace",
		"Baggage":                  "raw-baggage",
		"X-Stainless-Retry-Count":  "7",
	} {
		req.Header.Set(key, value)
	}
	inbound := req.Header.Clone()
	sa := &storedAuth{Auth: storedTokens{AccessToken: "trusted-access", Domain: "www.codebuddy.cn", ClientMode: oauthClientModeCLI}}
	backendChatHeaders(req, sa, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), nil, inbound)
	if got := req.Header.Get("Authorization"); got != "Bearer trusted-access" {
		t.Fatalf("Authorization = %q", got)
	}
	for _, key := range []string{"X-Refresh-Token", "X-DSH-Session-ID", "X-Session-ID", "Session-ID", "OpenAI-Conversation-ID", "X-OpenAI-Conversation-ID", "Cookie", "Forwarded", "X-Client-Platform", "X-Agent-Type", "X-Amzn-Trace-Id", "Sentry-Trace", "Baggage", "X-Stainless-Retry-Count"} {
		if got := req.Header.Get(key); got != "" {
			t.Errorf("%s leaked upstream: %q", key, got)
		}
	}
	if got := req.Header.Get("X-Conversation-ID"); got == "" || got == "old-conversation" {
		t.Fatalf("conversation ID was not replaced: %q", got)
	}
	if got := req.Header.Get("X-IDE-Version"); got != codeBuddyCLIVersion {
		t.Fatalf("X-IDE-Version = %q", got)
	}
	if got := req.Header.Get("traceparent"); got == "" || got == "spoofed-trace" {
		t.Fatalf("traceparent was not replaced: %q", got)
	}
}

func TestBackendChatHeadersAddsStableClientIdentity(t *testing.T) {
	makeHeaders := func() http.Header {
		req := httptest.NewRequest("POST", "https://example.com/v2/chat/completions", nil)
		backendChatHeaders(req, &storedAuth{}, []byte(`{"messages":[{"role":"user","content":"hi"}]}`), map[string]any{"canonical_session_id": "session-1"}, nil)
		return req.Header
	}

	first := makeHeaders()
	for name, want := range map[string]string{
		"X-Agent-Intent":      "craft",
		"X-Agent-Purpose":     "conversation",
		"X-CodeBuddy-Request": "1",
	} {
		if got := first.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := first.Get("X-Agent-Type"); got != "" {
		t.Errorf("X-Agent-Type = %q, absent from inspected Mac clients", got)
	}
	tracePattern := regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-01$`)
	traceParts := tracePattern.FindStringSubmatch(first.Get("traceparent"))
	if len(traceParts) != 3 {
		t.Fatalf("traceparent = %q", first.Get("traceparent"))
	}
	if got := first.Get("X-Trace-Id"); got != traceParts[1] {
		t.Errorf("X-Trace-Id = %q, want %q", got, traceParts[1])
	}
	if got := first.Get("X-B3-TraceId"); got != traceParts[1] {
		t.Errorf("X-B3-TraceId = %q, want %q", got, traceParts[1])
	}
	if got := first.Get("X-B3-SpanId"); got != traceParts[2] {
		t.Errorf("X-B3-SpanId = %q, want %q", got, traceParts[2])
	}
	if got := first.Get("X-B3-Sampled"); got != "1" {
		t.Errorf("X-B3-Sampled = %q", got)
	}
	wantB3Prefix := traceParts[1] + "-" + traceParts[2] + "-1-"
	if got := first.Get("b3"); !strings.HasPrefix(got, wantB3Prefix) || !regexp.MustCompile(`[0-9a-f]{16}$`).MatchString(got) {
		t.Errorf("b3 = %q, want prefix %q and 16-hex parent", got, wantB3Prefix)
	}
	idPattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	idHeaders := []string{
		"X-Request-ID",
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
		"acp-connection-id",
	}
	seen := make(map[string]string, len(idHeaders))
	for _, name := range idHeaders {
		value := first.Get(name)
		if !idPattern.MatchString(value) {
			t.Errorf("%s = %q, want a lowercase RFC 4122 v4 UUID", name, value)
		}
		if previous, exists := seen[value]; exists {
			t.Errorf("%s duplicates %s with value %q", name, previous, value)
		}
		seen[value] = name
	}

	second := makeHeaders()
	for _, name := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "acp-connection-id"} {
		if first.Get(name) != second.Get(name) {
			t.Errorf("%s changed across the same session/turn: %q != %q", name, first.Get(name), second.Get(name))
		}
	}
	for _, name := range []string{"X-Request-ID", "X-Conversation-Message-ID", "traceparent"} {
		if first.Get(name) == second.Get(name) {
			t.Errorf("%s was reused across requests: %q", name, first.Get(name))
		}
	}
}
