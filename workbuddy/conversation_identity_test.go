package main

import (
	"net/http"
	"regexp"
	"testing"
)

func mustConversationIdentity(t *testing.T, sa *storedAuth, body []byte, metadata map[string]any, headers http.Header) conversationIdentity {
	t.Helper()
	identity, err := deriveConversationIdentity(sa, body, metadata, headers)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestConversationIdentityStableAcrossOneSession(t *testing.T) {
	sa := &storedAuth{}
	sa.Auth.Domain = "www.workbuddy.ai"
	sa.Account.UID = "user-1"
	metadata := map[string]any{"canonical_session_id": "session-a"}
	firstBody := []byte(`{"messages":[{"role":"user","content":"first"}]}`)
	secondBody := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"first"}]}`)

	first := mustConversationIdentity(t, sa, firstBody, metadata, nil)
	retry := mustConversationIdentity(t, sa, firstBody, metadata, nil)
	second := mustConversationIdentity(t, sa, secondBody, metadata, nil)

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

	idOne := mustConversationIdentity(t, one, body, map[string]any{"canonical_session_id": "s1"}, nil)
	idOtherAccount := mustConversationIdentity(t, two, body, map[string]any{"canonical_session_id": "s1"}, nil)
	idOtherSession := mustConversationIdentity(t, one, body, map[string]any{"canonical_session_id": "s2"}, nil)
	if idOne.ConversationID == idOtherAccount.ConversationID || idOne.ConversationID == idOtherSession.ConversationID {
		t.Fatal("conversation identity was not scoped by account and session")
	}
}

func TestConversationIdentityScopesPersistedClientFamily(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	cli := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeCLI}, Account: storedAccount{UID: "user-1"}}
	desktop := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeWorkBuddy}, Account: storedAccount{UID: "user-1"}}
	cliID := mustConversationIdentity(t, cli, body, map[string]any{"canonical_session_id": "s1"}, nil)
	desktopID := mustConversationIdentity(t, desktop, body, map[string]any{"canonical_session_id": "s1"}, nil)
	if cliID.ConversationID == desktopID.ConversationID || cliID.ACPConnectionID == desktopID.ACPConnectionID {
		t.Fatal("client family change reused the old conversation namespace")
	}
}

func TestConversationIdentityUsesUserOnlyTurnOrdinal(t *testing.T) {
	sa := &storedAuth{}
	metadata := map[string]any{"canonical_session_id": "session-user-ordinal"}
	plain := []byte(`{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`)
	withNonUserWrappers := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"first"},{"role":"tool","content":"result"},{"role":"assistant","content":"ok"},{"role":"developer","content":"wrapper"},{"role":"user","content":"next"},{"role":"assistant","content":"postlude"}]}`)
	first := mustConversationIdentity(t, sa, plain, metadata, nil)
	wrapped := mustConversationIdentity(t, sa, withNonUserWrappers, metadata, nil)
	if first.ConversationRequestID != wrapped.ConversationRequestID {
		t.Fatalf("non-user insertion changed current user turn: %s != %s", first.ConversationRequestID, wrapped.ConversationRequestID)
	}
	third := mustConversationIdentity(t, sa, []byte(`{"messages":[{"role":"user","content":"first"},{"role":"user","content":"next"},{"role":"user","content":"next"}]}`), metadata, nil)
	if third.ConversationRequestID == first.ConversationRequestID {
		t.Fatal("a later repeated user message reused the previous user ordinal")
	}
}

func TestConversationIdentityIsKeyedByInstallationSecret(t *testing.T) {
	var firstKey, secondKey [installationSecretBytes]byte
	for i := range firstKey {
		firstKey[i] = 0x11
		secondKey[i] = 0x22
	}
	t.Cleanup(func() {
		var fixture [installationSecretBytes]byte
		for i := range fixture {
			fixture[i] = byte(i + 1)
		}
		installInstallationSecretForTest(fixture)
	})
	body := []byte(`{"messages":[{"role":"user","content":"same"}]}`)
	metadata := map[string]any{"canonical_session_id": "same-session"}
	installInstallationSecretForTest(firstKey)
	one := mustConversationIdentity(t, &storedAuth{}, body, metadata, nil)
	installInstallationSecretForTest(secondKey)
	two := mustConversationIdentity(t, &storedAuth{}, body, metadata, nil)
	if one.ConversationID == two.ConversationID || one.ConversationRequestID == two.ConversationRequestID || one.ACPConnectionID == two.ACPConnectionID {
		t.Fatal("installation secret did not scope stable identifiers")
	}
}

func TestStableWorkBuddyIDSeparatesPurposeAndPartBoundaries(t *testing.T) {
	var key [installationSecretBytes]byte
	for i := range key {
		key[i] = byte(0x80 + i)
	}
	if stableWorkBuddyID(key, "conversation", "ab", "c") == stableWorkBuddyID(key, "conversation", "a", "bc") {
		t.Fatal("length framing collision")
	}
	if stableWorkBuddyID(key, "conversation", "same") == stableWorkBuddyID(key, "turn", "same") {
		t.Fatal("purpose domain collision")
	}
}

func TestConversationIdentityFailsClosedWithoutInstallationSecret(t *testing.T) {
	var fixture [installationSecretBytes]byte
	for i := range fixture {
		fixture[i] = byte(i + 1)
	}
	t.Cleanup(func() { installInstallationSecretForTest(fixture) })
	clearInstallationSecretForTest()
	if _, err := deriveConversationIdentity(&storedAuth{}, []byte(`{"messages":[{"role":"user","content":"same"}]}`), nil, nil); err != errInstallationIdentity {
		t.Fatalf("error = %v", err)
	}
}

func TestConversationIdentityFallsBackToFirstUserTurn(t *testing.T) {
	first := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"start"}]}`)
	later := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"start"},{"role":"assistant","content":"ok"},{"role":"user","content":"next"}]}`)
	one := mustConversationIdentity(t, &storedAuth{}, first, nil, nil)
	two := mustConversationIdentity(t, &storedAuth{}, later, nil, nil)
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
		if err := backendChatHeaders(req, &storedAuth{}, body, nil, inbound); err != nil {
			t.Fatal(err)
		}
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
