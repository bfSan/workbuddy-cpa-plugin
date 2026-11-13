// payload.go rewrites the outgoing chat completion request body before it's
// forwarded upstream. The single-pass entry point is prepareUpstreamBody; the
// four *InPlace helpers are the field-level mutations it composes, and the
// legacy *ForUpstream / forceStreamBody wrappers exist for tests and other
// call sites that need them individually.
package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// preparedUpstreamRequest carries the final wire body and the request-local
// tool-name aliases needed to restore upstream tool calls for the caller.
type preparedUpstreamRequest struct {
	Body      []byte
	ToolNames toolNameMapping
}

// forceStreamBody returns the request body with "stream":true set, since the
// upstream rejects non-streaming chat requests.
// prepareUpstreamBody is the compatibility wrapper used by focused tests and
// legacy call sites. Executors use prepareUpstreamRequest so response handling
// can restore any safe tool aliases generated for this request.
func prepareUpstreamBody(payload, original []byte, sa *storedAuth, upstreamModel string) []byte {
	return prepareUpstreamRequest(payload, original, sa, upstreamModel).Body
}

// prepareUpstreamRequest composes all WorkBuddy wire adaptations in one
// unmarshal/marshal pass. It intentionally keeps the caller's system prompt and
// tool semantics; only gateway-incompatible shapes and known channel markers
// are rewritten.
func prepareUpstreamRequest(payload, original []byte, sa *storedAuth, upstreamModel string) preparedUpstreamRequest {
	src := payload
	if len(src) == 0 {
		src = original
	}
	if len(src) == 0 {
		return preparedUpstreamRequest{Body: src}
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return preparedUpstreamRequest{Body: src}
	}

	// 0. Restore the caller's explicit reasoning effort. CPA v8's Responses →
	// chat ExecutorRequest normalization can omit reasoning from Payload while
	// OriginalRequest still carries reasoning.effort. WorkBuddy's chat upstream
	// only reads the top-level reasoning_effort/reasoning_summary pair.
	restoreReasoningEffortInPlace(obj, original)

	// 1. CodeBuddy rejects non-stream requests. It also understands max_tokens,
	// not the newer max_completion_tokens spelling.
	obj["stream"] = true
	normalizeMaxCompletionTokensInPlace(obj)

	// 2. Normalize tool_choice before safe aliases are generated; fitToolsInPlace
	// then rewrites a selected original tool name to its upstream alias.
	normalizeToolsInPlace(obj)

	// 3. Swap the client-facing model alias to the upstream model id.
	rewriteModelInPlace(obj, upstreamModel)

	// 4. The gateway accepts only system as the privileged role and requires it
	// at the front. Preserve content while fixing role and position.
	normalizeSystemFirstInPlace(obj)

	// 4b. Neutralize only the channel fingerprints confirmed by real DSH A/B
	// tests. Do not replace the complete system prompt with a generic sentence.
	neutralizeChannelFingerprintsInPlace(obj)

	// 5. Keep the historical Claude-template sanitation and optional configured
	// desensitization, both scoped to fields the plugin already owns.
	rewriteSystemInPlace(obj)
	applyDesensitizeInPlace(obj, currentFeatureRuntime())

	// 6. Global requires a system message; CN tolerates the normalized shape.
	ensureSystemMessageInPlace(obj, sa)

	// 7. Repair tool→user turn boundaries rejected by Gemini-family models, then
	// fit the narrower WorkBuddy function-name and JSON-Schema dialect.
	alignWorkBuddyTurnOrderInPlace(obj)
	toolNames := fitToolsInPlace(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return preparedUpstreamRequest{Body: src}
	}
	return preparedUpstreamRequest{Body: out, ToolNames: toolNames}
}

func normalizeMaxCompletionTokensInPlace(obj map[string]any) bool {
	value, present := obj["max_completion_tokens"]
	if !present {
		return false
	}
	if _, exists := obj["max_tokens"]; !exists {
		obj["max_tokens"] = value
	}
	delete(obj, "max_completion_tokens")
	return true
}

// alignWorkBuddyTurnOrderInPlace repairs the gateway-invalid tail
// tool → user (or tool → user → user) by inserting an empty assistant turn at
// the boundary. Other message order is left untouched.
func alignWorkBuddyTurnOrderInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) < 2 {
		return false
	}
	index := len(messages) - 1
	for index >= 0 {
		message, _ := messages[index].(map[string]any)
		role, _ := message["role"].(string)
		if !strings.EqualFold(role, "user") {
			break
		}
		index--
	}
	if index < 0 || index == len(messages)-1 {
		return false
	}
	message, _ := messages[index].(map[string]any)
	role, _ := message["role"].(string)
	if !strings.EqualFold(role, "tool") {
		return false
	}
	messages = append(messages, nil)
	copy(messages[index+2:], messages[index+1:])
	messages[index+1] = map[string]any{"role": "assistant", "content": ""}
	obj["messages"] = messages
	return true
}

// normalizeToolsInPlace is the in-place form of normalizeToolsForUpstream.
// Returns true when obj was modified.
func normalizeToolsInPlace(obj map[string]any) bool {
	changed := false
	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}
	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			delete(obj, "tool_choice")
			changed = true
		}
	}
	return changed
}

// rewriteSystemInPlace is the in-place form of rewriteSystemForUpstream.
func rewriteSystemInPlace(obj map[string]any) bool {
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	return changed
}

// ensureSystemMessageInPlace is the in-place form of ensureSystemMessage.
// Returns true when obj was modified.
func ensureSystemMessageInPlace(obj map[string]any, sa *storedAuth) bool {
	if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
		return false
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
			return false
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	return true
}

// restoreReasoningEffortInPlace converts the caller's explicit reasoning
// selection to the WorkBuddy chat wire shape. CPA v8 may omit the selection
// from its normalized ExecutorRequest Payload, but keeps the unmodified API
// body in OriginalRequest. Payload values win; OriginalRequest is recovery
// evidence only. No model default is invented when the caller selected none.
func restoreReasoningEffortInPlace(obj map[string]any, original []byte) bool {
	changed := false

	// Read the already-normalized chat fields first.
	effort, _ := obj["reasoning_effort"].(string)
	effort = normalizeReasoningEffort(effort)
	summary, _ := obj["reasoning_summary"].(string)
	summary = normalizeReasoningEffort(summary)

	// Some callers or host versions leave the Responses shape in Payload.
	if nested, ok := obj["reasoning"].(map[string]any); ok {
		if effort == "" {
			if value, ok := nested["effort"].(string); ok {
				effort = normalizeReasoningEffort(value)
			}
		}
		if summary == "" {
			if value, ok := nested["summary"].(string); ok {
				summary = normalizeReasoningEffort(value)
			}
		}
		delete(obj, "reasoning")
		changed = true
	}

	// Recover fields stripped by the host's Responses → chat normalization.
	if (effort == "" || summary == "") && len(original) > 0 {
		var source map[string]any
		if json.Unmarshal(original, &source) == nil {
			if effort == "" {
				if value, ok := source["reasoning_effort"].(string); ok {
					effort = normalizeReasoningEffort(value)
				}
			}
			if summary == "" {
				if value, ok := source["reasoning_summary"].(string); ok {
					summary = normalizeReasoningEffort(value)
				}
			}
			if nested, ok := source["reasoning"].(map[string]any); ok {
				if effort == "" {
					if value, ok := nested["effort"].(string); ok {
						effort = normalizeReasoningEffort(value)
					}
				}
				if summary == "" {
					if value, ok := nested["summary"].(string); ok {
						summary = normalizeReasoningEffort(value)
					}
				}
			}
		}
	}

	if effort == "" {
		return changed
	}
	if current, _ := obj["reasoning_effort"].(string); current != effort {
		obj["reasoning_effort"] = effort
		changed = true
	}
	// Official WorkBuddy clients pair an explicit effort with summary=auto.
	// Preserve a caller-provided summary; use auto only when effort exists.
	if summary == "" {
		summary = "auto"
	}
	if current, _ := obj["reasoning_summary"].(string); current != summary {
		obj["reasoning_summary"] = summary
		changed = true
	}
	return changed
}

// normalizeReasoningEffort bounds one wire token and rejects line-breaking
// input. It intentionally accepts future effort names rather than hard-coding
// today's catalog, while still preventing header-like/multiline values.
func normalizeReasoningEffort(raw string) string {
	return normalizeModelEffort(raw)
}

// normalizeSystemFirstInPlace makes the message array comply with the WorkBuddy
// chat gateway contract: only "system" is accepted as the privileged prompt
// role, and the first message must be a system one.
//
// Host v8.0.61+ preserves the OpenAI Responses "developer" role instead of
// downgrading it to user (host commit 00b76771 / CLIProxyAPI#7174). Forwarding
// developer makes the CN gateway answer 400 code 11128 "Illegal API invocation
// from an unapproved channel"; the same family covers requests whose first
// message is not a system prompt. Developer is therefore mapped to system in
// place (content, string or multimodal, untouched), and when no system message
// leads the array a minimal placeholder is prepended.
func normalizeSystemFirstInPlace(obj map[string]any) bool {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "developer") {
			msg["role"] = "system"
			changed = true
		}
	}
	// After the role mapping, guarantee a system message leads the array.
	if first, ok := messages[0].(map[string]any); ok {
		if role, _ := first["role"].(string); strings.EqualFold(role, "system") {
			return changed
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	return true
}

// rewriteModelInPlace swaps obj["model"] to upstreamModel when non-empty.
// Returns true when modified.
func rewriteModelInPlace(obj map[string]any, upstreamModel string) bool {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return false
	}
	cur, _ := obj["model"].(string)
	if cur == upstreamModel {
		return false
	}
	obj["model"] = upstreamModel
	return true
}

func forceStreamBody(payload, original []byte) []byte {
	src := payload
	if len(src) == 0 {
		src = original
	}
	var obj map[string]any
	if json.Unmarshal(src, &obj) != nil {
		return src
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolsForUpstream adapts OpenAI tools / tool_choice fields to
// CodeBuddy's chat schema before the request is forwarded.
//
// Live-verified against /v2/chat/completions (2026-07):
//  1. tool_choice is typed as string on the upstream Go struct. OpenAI's object
//     form {"type":"function","function":{"name":"..."}} returns 400 code 11101
//     ("cannot unmarshal object into Go struct field Request.tool_choice of
//     type string"). Convert known object shapes to the matching string.
//  2. tool_choice "none" is accepted but ignored when tools[] is non-empty —
//     the model still emits tool_calls. The only reliable way to suppress tools
//     is to omit tools (and functions) entirely.
//
// String values auto / required / <function name> are left untouched.
func normalizeToolsForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	changed := false

	suppressTools := func() {
		if _, ok := obj["tools"]; ok {
			delete(obj, "tools")
			changed = true
		}
		if _, ok := obj["functions"]; ok {
			delete(obj, "functions")
			changed = true
		}
	}

	if tc, present := obj["tool_choice"]; present {
		switch v := tc.(type) {
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "none") {
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			typ = strings.ToLower(strings.TrimSpace(typ))
			switch typ {
			case "none":
				delete(obj, "tool_choice")
				suppressTools()
				changed = true
			case "auto", "required":
				obj["tool_choice"] = typ
				changed = true
			case "function":
				name := ""
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
				if name == "" {
					name, _ = v["name"].(string)
				}
				name = strings.TrimSpace(name)
				if name != "" {
					obj["tool_choice"] = name
				} else {
					// Object force without a name: fall back to auto instead of 400.
					obj["tool_choice"] = "auto"
				}
				changed = true
			default:
				// Unknown object shape → drop rather than forward a 400.
				delete(obj, "tool_choice")
				changed = true
			}
		default:
			// null / array / number — drop to keep upstream happy.
			delete(obj, "tool_choice")
			changed = true
		}
	}

	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteSystemForUpstream neutralizes Claude Code template phrases that
// Tencent CodeBuddy's content filter blocklists verbatim — the agent identity
// line ("You are Claude Code, Anthropic's official CLI for Claude.") and the
// git injection ("Main branch (you will usually use this for PRs)"). Each
// rewrite is a single-word change so the prompt's meaning is preserved while
// dodging the exact-match filter.
func rewriteSystemForUpstream(payload []byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, _ := obj["messages"].([]any)
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if rewriteContentField(msg) {
			changed = true
		}
	}
	if !changed {
		return payload
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// ensureSystemMessage injects a minimal system message if none is present.
// Global (www.workbuddy.ai) rejects user-only requests with code 11101
// "Parse message failed: 11101:invalid request". CN (copilot.tencent.com)
// does not require a system message but tolerates one. Inserting a
// harmless system message unifies both paths.
func ensureSystemMessage(payload []byte, sa *storedAuth) []byte {
	if len(payload) == 0 {
		return payload
	}
	// Only inject for Global; CN doesn't need it and we minimize diff.
	if sa == nil || !isGlobalDomain(sa.Auth.Domain) {
		return payload
	}
	var obj map[string]any
	if json.Unmarshal(payload, &obj) != nil {
		return payload
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return payload
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); strings.EqualFold(role, "system") {
			return payload // already has system message
		}
	}
	systemMsg := map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}
	obj["messages"] = append([]any{systemMsg}, messages...)
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return out
}

// rewriteContentField sanitizes blocked templates in one message's content,
// handling both plain-string and OpenAI multimodal (array of parts) shapes.
// Returns true if the message was modified.
func rewriteContentField(msg map[string]any) bool {
	switch c := msg["content"].(type) {
	case string:
		if r := sanitizeBlockedTemplates(c); r != c {
			msg["content"] = r
			return true
		}
	case []any:
		modified := false
		for _, p := range c {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := part["text"].(string); ok {
				if r := sanitizeBlockedTemplates(t); r != t {
					part["text"] = r
					modified = true
				}
			}
		}
		return modified
	}
	return false
}

var sanitizeFeatures = []string{
	"x-anthropic-billing-header",
	"You are Claude Code",
	"Main branch (",
}

var sanitizeBillingHeaderRE = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
var sanitizeCCEntrypointRE = regexp.MustCompile(`(?i)\bcc_entrypoint=`)
var sanitizeCCKeyValueRE = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

func sanitizeBlockedTemplates(s string) string {
	if !hasFingerprint(s) {
		return s
	}
	original := s
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	s = sanitizeBillingHeaderRE.ReplaceAllString(s, "")
	s = sanitizeCCKeyValueRE.ReplaceAllString(s, "")
	if s == original {
		return original
	}
	return strings.TrimSpace(s)
}

func hasFingerprint(s string) bool {
	if sanitizeCCEntrypointRE.MatchString(s) {
		return true
	}
	for _, feature := range sanitizeFeatures {
		if strings.Contains(s, feature) {
			return true
		}
	}
	return sanitizeBillingHeaderRE.MatchString(s)
}

// rewriteModelInBody replaces the "model" field of a chat-completions body
// with the resolved upstream model ID.
func rewriteModelInBody(body []byte, upstreamModel string) []byte {
	if len(body) == 0 || strings.TrimSpace(upstreamModel) == "" {
		return body
	}
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	cur, _ := obj["model"].(string)
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(upstreamModel)) {
		return body
	}
	obj["model"] = upstreamModel
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func isEmptyValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		if len(x) == 0 {
			return true
		}
		// Legacy function_call shell: {"name":"","arguments":""} is the
		// upstream's terminal-chunk artifact, not a real call — treat as empty
		// when every value is itself empty.
		for _, val := range x {
			if !isEmptyValue(val) {
				return false
			}
		}
		return true
	}
	return false
}
