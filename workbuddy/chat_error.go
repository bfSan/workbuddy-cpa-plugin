package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Classifications use WorkBuddy evidence, not another provider's numeric codes.
// In particular 6004 is the observed model-frequency rejection and 12153 is a
// revoked offline session. Code 110 alone has no meaning here.
type chatFailure struct {
	Reason            cooldownReason
	Scope             string
	Evidence          string
	CredentialInvalid bool
}

const (
	quotaScopeModel                            = "account_model"
	quotaScopeAccount                          = "account"
	cooldownReasonCallQuota     cooldownReason = "call_quota"
	cooldownReasonCreditQuota   cooldownReason = "credit_quota"
	cooldownReasonSharedCredits cooldownReason = "shared_credits_exhausted"
)

// chatErrorFields reads error fields only: never classify assistant content,
// request echoes, debug details or arbitrary nested strings as quota evidence.
func chatErrorFields(body string) (text string, code string, scope string) {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &obj) != nil {
		if strings.HasPrefix(strings.TrimSpace(body), "{") || strings.HasPrefix(strings.TrimSpace(body), "[") {
			return "", "", ""
		}
		return strings.ToLower(strings.TrimSpace(body)), "", ""
	}
	if _, ok := obj["choices"]; ok {
		return "", "", ""
	}
	read := func(raw json.RawMessage) string {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			return n.String()
		}
		return ""
	}
	parts := []string{read(obj["msg"]), read(obj["message"]), read(obj["type"])}
	code, scope = read(obj["code"]), read(obj["scope"])
	if raw := obj["error"]; len(raw) > 0 && string(raw) != "null" {
		if s := read(raw); s != "" {
			parts = append(parts, s)
		} else {
			nestedText, nestedCode, nestedScope := chatErrorFields(string(raw))
			parts = append(parts, nestedText)
			if code == "" || code == "0" {
				code = nestedCode
			}
			if scope == "" {
				scope = nestedScope
			}
		}
	}
	return strings.ToLower(strings.Join(parts, " ")), code, strings.ToLower(scope)
}

func chatContains(s string, patterns ...string) bool {
	for _, p := range patterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

func classifyChatFailure(status int, body string) chatFailure {
	text, code, scope := chatErrorFields(body)
	// Request/capability errors are not quota, even if their copy mentions a limit.
	if code == "11101" || code == "11102" || code == "11103" || code == "11115" || code == "11128" || status == 413 ||
		chatContains(text, "context length", "context window", "prompt too long", "prompt is too long", "too many tokens", "输入过长", "上下文过长") {
		return chatFailure{}
	}
	if code == "12153" || chatContains(text, "offline user session not found", "invalid token", "token expired", "token has expired", "invalid_grant", "登录已失效", "登录失效") {
		return chatFailure{Scope: quotaScopeAccount, Evidence: "credential_rejected", CredentialInvalid: true}
	}
	if code == "6004" && (status == 429 || status == 200 || status == 0) {
		return chatFailure{Reason: cooldownReasonRateLimit, Scope: quotaScopeModel, Evidence: "workbuddy_6004"}
	}
	// Negations, informational quota fields and bare numeric codes are not proof.
	if chatContains(text, "not exhausted", "not exceeded", "not reached", "quota available", "未耗尽", "未达到", "尚未用尽") {
		return chatFailure{}
	}
	credit := chatContains(text, "insufficient credit", "insufficient shared credit", "insufficient account credit", "no credit left", "no credits left", "credit exhausted", "credits exhausted", "out of credit", "credit not enough", "not enough credit", "积分不足", "积分耗尽", "积分已耗尽", "积分用完", "积分已用尽", "积分用尽", "没有积分", "余额不足")
	shared := chatContains(text, "shared credit", "account credit", "account-wide credit", "共享积分", "账户积分", "账号积分", "帐户积分") || scope == "account" || scope == "shared"
	depleted := chatContains(text, "credit exhausted", "credits exhausted", "credits depleted", "credit balance exhausted", "out of credit", "no credits left", "no credit left", "积分耗尽", "积分已耗尽", "积分用完", "积分已用尽", "积分用尽", "没有积分")
	credit = credit || depleted
	if credit {
		// Insufficient for THIS call does not prove a shared pool is empty.
		if shared && depleted {
			return chatFailure{Reason: cooldownReasonSharedCredits, Scope: quotaScopeAccount, Evidence: "explicit_shared_credit_exhaustion"}
		}
		return chatFailure{Reason: cooldownReasonCreditQuota, Scope: quotaScopeModel, Evidence: "credit_refusal_scope_unproven"}
	}
	daily := chatContains(text, "daily", "per day", "今日", "每日", "当天", "每天")
	quota := chatContains(text, "quota", "allowance", "limit", "额度", "上限", "次数", "用量")
	exhausted := chatContains(text, "exceeded", "exhausted", "reached", "used up", "已达", "达到", "用尽", "耗尽", "超出", "超过", "用完")
	if (daily && quota && exhausted) || chatContains(text, "quota exceeded", "quota exhausted", "quota reached", "call limit reached", "request limit reached", "调用次数已达上限", "调用额度已用尽", "请求次数已达上限", "额度不足", "额度用尽", "额度已用尽") {
		return chatFailure{Reason: cooldownReasonCallQuota, Scope: quotaScopeModel, Evidence: "explicit_call_allowance_rejection"}
	}
	if status == 429 || chatContains(text, "rate limit", "too many requests", "throttl", "频率限制", "请求过于频繁") {
		return chatFailure{Reason: cooldownReasonRateLimit, Scope: quotaScopeModel, Evidence: "temporary_rate_limit"}
	}
	return chatFailure{}
}

// chatUpstreamError deliberately removes BOTH HTTP status and raw status-like
// text for plugin-owned quota states. CPA otherwise upgrades business 401/403
// (and even model 429) into credential-wide cooldown. The existing transport
// compatibility marker suppresses that host cooldown; this plugin owns the
// actual bounded hold. Never include arbitrary upstream text in this branch.
func chatUpstreamError(status int, body string) *upstreamStatusError {
	f := classifyChatFailure(status, body)
	if f.Reason != "" {
		return &upstreamStatusError{code: "workbuddy_quota_hold", message: fmt.Sprintf("workbuddy quota hold: reason=%s scope=%s; retry after the local hold (unexpected EOF)", f.Reason, f.Scope)}
	}
	if f.CredentialInvalid {
		return &upstreamStatusError{code: "workbuddy_credential_invalid", status: http.StatusUnauthorized, message: "upstream 401: workbuddy credential invalid; re-login required"}
	}
	text, _, _ := chatErrorFields(body)
	if status == http.StatusPaymentRequired || status == http.StatusForbidden ||
		((status == http.StatusUnauthorized) && chatContains(text, "quota", "credit", "allowance", "额度", "积分", "限额")) {
		// Ambiguous billing/permission evidence is not an invalid credential and
		// is not enough to create a local hold either.
		return &upstreamStatusError{code: "workbuddy_scope_unconfirmed", message: "workbuddy upstream rejection; quota or access scope unconfirmed (unexpected EOF)"}
	}
	return &upstreamStatusError{status: status, message: fmt.Sprintf("upstream %d: %s", status, truncateRedacted(body, 200))}
}

// chatStreamFrameError detects only explicit error envelopes on an HTTP-success
// stream. Normal choice text containing quota/error words is never inspected.
func chatStreamFrameError(content string) *upstreamStatusError {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &obj) != nil || obj == nil {
		return nil
	}
	if _, ok := obj["choices"]; ok {
		return nil
	}
	_, code, _ := chatErrorFields(content)
	hasError := len(obj["error"]) > 0 && string(obj["error"]) != "null" && string(obj["error"]) != `""`
	if n, err := strconv.Atoi(code); err == nil && n != 0 {
		hasError = true
	}
	var typ string
	_ = json.Unmarshal(obj["type"], &typ)
	if typ == "error" {
		hasError = true
	}
	f := classifyChatFailure(200, content)
	// Some WorkBuddy limit frames contain only msg/message, with no numeric
	// code. Require explicit rejection semantics, no choices and no success code.
	if code == "" && (len(obj["msg"]) > 0 || len(obj["message"]) > 0) && (f.Reason != "" || f.CredentialInvalid) {
		hasError = true
	}
	if !hasError {
		return nil
	}
	if f.Reason != "" || f.CredentialInvalid {
		return chatUpstreamError(200, content)
	}
	return &upstreamStatusError{message: "workbuddy upstream error frame (unexpected EOF)"}
}
