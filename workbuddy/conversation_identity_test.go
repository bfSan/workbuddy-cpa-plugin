package main

import (
	"net/http"
	"regexp"
	"testing"
)

func TestConversationIdentityStableAcrossOneSession(t *testing.T) {
	sa := &storedAuth{}
	sa.Auth.Domain = "www.workbuddy.ai"
	sa.Account.UID = "user-1"
	metadata := map[string]any{"canonical_session_id": "session-a"}
	firstBody := []byte(`{"messages":[{"role":"user","content":"first"}]}`)
	secondBody := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"first"}]}`)

	first := deriveConversationIdentity(sa, firstBody, metadata, nil)
	retry := deriveConversationIdentity(sa, firstBody, metadata, nil)
	second := deriveConversationIdentity(sa, secondBody, metadata, nil)

	if first.ConversationID != retry.ConversationID || first.ConversationID != second.ConversationID {
		t.Fatalf("conversation id changed: first=%s retry=%s second=%s", first.ConversationID, retry.ConversationID, second.ConversationID)
	}
	if first.ACPConnectionID != retry.ACPConnectionID || first.ACPConnectionID != second.ACPConnectionID {
		t.Fatal("ACP connection id changed inside one downstream session")
	}
	if first.ConversationRequestID != retry.ConversationRequestID {
		t.Fatalf("same turn id changed: %s != %s", first.ConversationRequestID, retry.ConversationRequestID)
	}
	if first.ConversationRequestID == second.ConversationRequestID {
		t.Fatal("different user turns reused one conversation request id")
	}
	if first.RequestID == retry.RequestID || first.ConversationMessageID == retry.ConversationMessageID {
		t.Fatal("transport request/message ids must remain unique across retries")
	}
	for _, value := range []string{first.RequestID, first.ConversationID, first.ConversationRequestID, first.ConversationMessageID, first.ACPConnectionID} {
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(value) {
			t.Fatalf("identity %q is not a lowercase RFC 4122 v4 UUID", value)
		}
	}
}

func TestConversationIdentityScopesAccountAndSession(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	one := &storedAuth{}
	one.Auth.Domain = "www.workbuddy.ai"
	one.Account.UID = "user-1"
	two := &storedAuth{}
	two.Auth.Domain = "www.workbuddy.ai"
	two.Account.UID = "user-2"

	idOne := deriveConversationIdentity(one, body, map[string]any{"canonical_session_id": "s1"}, nil)
	idOtherAccount := deriveConversationIdentity(two, body, map[string]any{"canonical_session_id": "s1"}, nil)
	idOtherSession := deriveConversationIdentity(one, body, map[string]any{"canonical_session_id": "s2"}, nil)
	if idOne.ConversationID == idOtherAccount.ConversationID || idOne.ConversationID == idOtherSession.ConversationID {
		t.Fatal("conversation identity was not scoped by account and session")
	}
}

func TestConversationIdentityScopesPersistedClientFamily(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	cli := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeCLI}, Account: storedAccount{UID: "user-1"}}
	desktop := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeWorkBuddy}, Account: storedAccount{UID: "user-1"}}
	cliID := deriveConversationIdentity(cli, body, map[string]any{"canonical_session_id": "s1"}, nil)
	desktopID := deriveConversationIdentity(desktop, body, map[string]any{"canonical_session_id": "s1"}, nil)
	if cliID.ConversationID == desktopID.ConversationID || cliID.ACPConnectionID == desktopID.ACPConnectionID {
		t.Fatal("client family change reused the old conversation namespace")
	}
}

func TestConversationIdentityFallsBackToFirstUserTurn(t *testing.T) {
	first := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"start"}]}`)
	later := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"start"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`)
	one := deriveConversationIdentity(&storedAuth{}, first, nil, nil)
	two := deriveConversationIdentity(&storedAuth{}, later, nil, nil)
	if one.ConversationID != two.ConversationID {
		t.Fatalf("history-derived conversation changed: %s != %s", one.ConversationID, two.ConversationID)
	}
	if one.ConversationRequestID == two.ConversationRequestID {
		t.Fatal("history-derived turn id did not advance")
	}
}

func TestBackendChatHeadersUsesInboundSession(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	inbound := make(http.Header)
	inbound.Set("X-DSH-Session-ID", "dsh-session")
	makeRequest := func() *http.Request {
		req, err := http.NewRequest(http.MethodPost, "https://example.test/v2/chat/completions", nil)
		if err != nil {
			t.Fatal(err)
		}
		backendChatHeaders(req, &storedAuth{}, body, nil, inbound)
		return req
	}
	first := makeRequest()
	second := makeRequest()
	if first.Header.Get("X-Conversation-ID") != second.Header.Get("X-Conversation-ID") {
		t.Fatal("inbound session did not produce a stable conversation id")
	}
	if first.Header.Get("X-Conversation-Request-ID") != second.Header.Get("X-Conversation-Request-ID") {
		t.Fatal("same turn did not produce a stable turn id")
	}
	if first.Header.Get("X-Request-ID") == second.Header.Get("X-Request-ID") {
		t.Fatal("request id was unexpectedly reused")
	}
}
