package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

var conversationMetadataKeys = []string{
	"canonical_session_id",
	"execution_session_id",
	"derived_session_id",
	"session_id",
	"conversation_id",
}

var conversationHeaderKeys = []string{
	"X-DSH-Session-ID",
	"X-Session-ID",
	"Session-ID",
	"X-Conversation-ID",
	"OpenAI-Conversation-ID",
}

type conversationIdentity struct {
	RequestID             string
	ConversationID        string
	ConversationRequestID string
	ConversationMessageID string
	ACPConnectionID       string
}

// backendChatHeaders applies the ordinary account/profile headers and then the
// chat-only request identity. Conversation and turn IDs remain stable for the
// same downstream session/turn; transport request and message IDs remain fresh
// for each retry, matching the real WorkBuddy client lifecycle.
func backendChatHeaders(req *http.Request, sa *storedAuth, body []byte, metadata map[string]any, inbound http.Header) error {
	profile := wireProfileForStoredAuth(sa)
	identity, err := deriveConversationIdentityWithProfile(sa, profile, body, metadata, inbound)
	if err != nil {
		return err
	}
	backendHeadersWithProfile(req, sa, profile)
	applyWireChatIntentHeaders(req)
	req.Header.Set("X-Request-ID", identity.RequestID)
	req.Header.Set("X-Conversation-ID", identity.ConversationID)
	req.Header.Set("X-Conversation-Request-ID", identity.ConversationRequestID)
	req.Header.Set("X-Conversation-Message-ID", identity.ConversationMessageID)
	req.Header.Set("acp-connection-id", identity.ACPConnectionID)
	return nil
}

func deriveConversationIdentity(sa *storedAuth, body []byte, metadata map[string]any, headers http.Header) (conversationIdentity, error) {
	return deriveConversationIdentityWithProfile(sa, wireProfileForStoredAuth(sa), body, metadata, headers)
}

func deriveConversationIdentityWithProfile(sa *storedAuth, profile workBuddyWireProfile, body []byte, metadata map[string]any, headers http.Header) (conversationIdentity, error) {
	secret, err := currentInstallationSecret()
	if err != nil {
		return conversationIdentity{}, errInstallationIdentity
	}
	namespace := accountIdentityNamespaceWithProfile(sa, profile)
	conversationKey, turnKey := requestConversationKeys(body, metadata, headers)
	if conversationKey == "" {
		conversationKey = "fallback:" + randomHex(16)
	}
	conversationID := stableWorkBuddyID(secret, "conversation", namespace, conversationKey)
	if turnKey == "" {
		turnKey = randomHex(16)
	}
	return conversationIdentity{
		RequestID:             randomWorkBuddyUUID(),
		ConversationID:        conversationID,
		ConversationRequestID: stableWorkBuddyID(secret, "turn", namespace, conversationID, turnKey),
		ConversationMessageID: randomWorkBuddyUUID(),
		ACPConnectionID:       stableWorkBuddyID(secret, "acp-connection", namespace, conversationKey),
	}, nil
}

func accountIdentityNamespace(sa *storedAuth) string {
	return accountIdentityNamespaceWithProfile(sa, wireProfileForStoredAuth(sa))
}

func accountIdentityNamespaceWithProfile(sa *storedAuth, profile workBuddyWireProfile) string {
	if sa == nil {
		return "anonymous"
	}
	parts := []string{
		strings.ToLower(strings.TrimSpace(sa.Auth.Domain)),
		profile.Mode,
		strings.TrimSpace(sa.Account.UID),
		strings.TrimSpace(sa.Account.EnterpriseID),
	}
	return strings.Join(parts, "\x00")
}

func requestConversationKeys(body []byte, metadata map[string]any, headers http.Header) (string, string) {
	for _, key := range conversationMetadataKeys {
		if value := stringValue(metadata[key]); value != "" {
			return "metadata:" + key + ":" + value, latestUserTurnKey(body)
		}
	}
	for _, key := range conversationHeaderKeys {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return "header:" + strings.ToLower(key) + ":" + value, latestUserTurnKey(body)
		}
	}

	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		for _, key := range []string{"conversation_id", "session_id", "prompt_cache_key"} {
			if value := stringValue(obj[key]); value != "" {
				return "body:" + key + ":" + value, latestUserTurnKeyFromObject(obj)
			}
		}
		if first := firstUserContent(obj); first != nil {
			return "first-user:" + jsonDigest(first), latestUserTurnKeyFromObject(obj)
		}
	}
	return "", latestUserTurnKey(body)
}

func latestUserTurnKey(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	return latestUserTurnKeyFromObject(obj)
}

func latestUserTurnKeyFromObject(obj map[string]any) string {
	ordinal := 0
	latest := ""
	for _, raw := range messageList(obj) {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "user") {
			latest = fmt.Sprintf("user:%d:%s", ordinal, jsonDigest(message["content"]))
			ordinal++
		}
	}
	return latest
}

func firstUserContent(obj map[string]any) any {
	for _, raw := range messageList(obj) {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if strings.EqualFold(strings.TrimSpace(role), "user") {
			return message["content"]
		}
	}
	return nil
}

func messageList(obj map[string]any) []any {
	if messages, ok := obj["messages"].([]any); ok {
		return messages
	}
	if input, ok := obj["input"].([]any); ok {
		return input
	}
	return nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func jsonDigest(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func stableWorkBuddyID(secret [installationSecretBytes]byte, parts ...string) string {
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write([]byte("workbuddy-stable-id-v1"))
	var length [4]byte
	for _, part := range parts {
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		_, _ = mac.Write(length[:])
		_, _ = mac.Write([]byte(part))
	}
	raw := append([]byte(nil), mac.Sum(nil)[:16]...)
	return formatWorkBuddyUUID(raw)
}

func randomWorkBuddyUUID() string {
	return formatWorkBuddyUUID(randomBytes(16))
}

func formatWorkBuddyUUID(raw []byte) string {
	if len(raw) < 16 {
		return "00000000-0000-4000-8000-000000000000"
	}
	bytes := append([]byte(nil), raw[:16]...)
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}
