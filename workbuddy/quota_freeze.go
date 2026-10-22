package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	quotaProbeTTL   = 15 * time.Minute
	quotaMaxHorizon = 24 * time.Hour
)

// Only explicitly zoned upstream reset copy is accepted. A daily allowance does
// not imply a midnight reset, and the host/browser timezone is irrelevant.
var quotaResetCopy = regexp.MustCompile(`(?i)(\d{4}-\d{2}-\d{2})[ T](\d{2}:\d{2}:\d{2})\s*UTC([+-]\d{1,2}(?::\d{2})?)\s*重置`)

func quotaRetryAt(headers http.Header, body string, now time.Time) (time.Time, string) {
	valid := func(t time.Time) bool { return t.After(now) && !t.After(now.Add(quotaMaxHorizon)) }
	if s := strings.TrimSpace(headers.Get("Retry-After")); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 && n <= int64(quotaMaxHorizon/time.Second) {
			return now.Add(time.Duration(n) * time.Second), "upstream_retry_after"
		}
		if t, err := http.ParseTime(s); err == nil && valid(t) {
			return t, "upstream_retry_after"
		}
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(headers.Get("Retry-After-Ms")), 10, 64); err == nil && n > 0 && n <= int64(quotaMaxHorizon/time.Millisecond) {
		return now.Add(time.Duration(n) * time.Millisecond), "upstream_retry_after"
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Reset")), 10, 64); err == nil && n > 0 {
		// This header is an absolute epoch. Never guess units for relative times.
		t := time.Unix(n, 0)
		if n >= 1e12 {
			t = time.UnixMilli(n)
		}
		if valid(t) {
			return t, "upstream_reset"
		}
	}
	text, _, _ := chatErrorFields(body)
	if m := quotaResetCopy.FindStringSubmatch(text); len(m) == 4 {
		offset := m[3]
		if !strings.Contains(offset, ":") {
			h, _ := strconv.Atoi(offset[1:])
			offset = fmt.Sprintf("%s%02d:00", offset[:1], h)
		}
		if t, err := time.Parse("2006-01-02 15:04:05 -07:00", m[1]+" "+m[2]+" "+offset); err == nil && valid(t) {
			return t, "upstream_reset"
		}
	}
	return time.Time{}, "bounded_ttl_probe"
}

// recordUpstreamFailureWithHeaders is also used for explicit HTTP-200 SSE error
// frames, before any lossy redaction/wrapping. No network or auth-file mutation.
func recordUpstreamFailureWithHeaders(authID, model string, status int, body string, headers http.Header) {
	f := classifyChatFailure(status, body)
	if f.Reason == "" {
		return
	}
	now := cooldownNowFn()
	until, recovery := quotaRetryAt(headers, body, now)
	if until.IsZero() {
		ttl := quotaProbeTTL
		if f.Reason == cooldownReasonRateLimit {
			ttl = modelCooldownRateLimit
		}
		until = now.Add(ttl)
	}
	markQuotaFreeze(authID, model, f, until, recovery)
}

func markQuotaFreeze(authID, model string, f chatFailure, until time.Time, recovery string) {
	authID, model = strings.TrimSpace(authID), strings.TrimSpace(model)
	if authID == "" || f.Reason == "" {
		return
	}
	if f.Scope == quotaScopeAccount {
		// An account-wide quota hold requires proven shared-credit exhaustion.
		if f.Reason != cooldownReasonSharedCredits {
			return
		}
		model = ""
	} else {
		if model == "" {
			return
		}
		f.Scope = quotaScopeModel
	}
	now := cooldownNowFn()
	if !until.After(now) {
		return
	}
	if until.After(now.Add(quotaMaxHorizon)) {
		until = now.Add(quotaProbeTTL)
		recovery = "bounded_ttl_probe"
	}
	key := modelCooldownKey{AuthID: authID, ModelID: model}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	cooldownSweepLocked(now)
	occurred := now
	if prev, ok := cooldownTable[key]; ok {
		occurred = prev.OccurredAt
		if occurred.IsZero() {
			occurred = prev.UpdatedAt
		}
		// A weaker signal must not overwrite a confirmed allowance hold, nor
		// may any re-mark shorten a previously declared upstream reset.
		if quotaReasonStrength(prev.Reason) > quotaReasonStrength(f.Reason) {
			return
		}
		if prev.Until.After(until) {
			until = prev.Until
			recovery = prev.RecoveryCondition
		}
	} else if len(cooldownTable) >= modelCooldownMaxEntries {
		oldest := modelCooldownKey{}
		var earliest time.Time
		for k, v := range cooldownTable {
			if earliest.IsZero() || v.Until.Before(earliest) {
				oldest, earliest = k, v.Until
			}
		}
		delete(cooldownTable, oldest)
	}
	cooldownTable[key] = modelCooldownEntry{
		Reason: f.Reason, Scope: f.Scope, Until: until, NextProbeAt: until,
		UpdatedAt: now, OccurredAt: occurred, RecoveryCondition: recovery, Evidence: f.Evidence,
	}
}

func quotaReasonStrength(reason cooldownReason) int {
	switch reason {
	case cooldownReasonSharedCredits:
		return 3
	case cooldownReasonCallQuota, cooldownReasonCreditQuota:
		return 2
	case cooldownReasonRateLimit:
		return 1
	default:
		return 0
	}
}

func quotaFreezeFor(authID, model string) (modelCooldownEntry, bool) {
	authID, model = strings.TrimSpace(authID), strings.TrimSpace(model)
	if authID == "" {
		return modelCooldownEntry{}, false
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	for _, key := range []modelCooldownKey{{AuthID: authID}, {AuthID: authID, ModelID: model}} {
		if entry, ok := cooldownTable[key]; ok {
			if entry.Until.After(now) {
				return entry, true
			}
			delete(cooldownTable, key)
		}
	}
	return modelCooldownEntry{}, false
}

func quotaFreezeError(authID, model string) *upstreamStatusError {
	entry, ok := quotaFreezeFor(authID, model)
	if !ok {
		return nil
	}
	return &upstreamStatusError{code: "workbuddy_quota_hold", message: fmt.Sprintf("workbuddy quota hold: reason=%s scope=%s next_probe_at=%s recovery_condition=%s (unexpected EOF)", entry.Reason, entry.Scope, entry.NextProbeAt.UTC().Format(time.RFC3339), entry.RecoveryCondition)}
}

// guardExecutorQuota checks the host-selected pair; it never chooses an account.
// Call after resolving aliases AND again immediately before upstream dispatch,
// including from queued goroutines. Existing manual-disable guards stay separate.
func guardExecutorQuota(authID, upstreamModel string) []byte {
	if err := quotaFreezeError(authID, upstreamModel); err != nil {
		return errorEnvelopeFor(err)
	}
	return nil
}

// observeQuotaCredits accepts fresh billing evidence only. A zero balance alone
// is NOT an account freeze: WorkBuddy rate-zero models may still be usable.
// Positive balance only clears a shared-credit hold, never daily/model limits.
func observeQuotaCredits(authID string, cr *creditsSummary) {
	if cr == nil || cr.TotalRemain <= 0 {
		return
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	key := modelCooldownKey{AuthID: strings.TrimSpace(authID)}
	if entry, ok := cooldownTable[key]; ok && entry.Reason == cooldownReasonSharedCredits {
		delete(cooldownTable, key)
	}
}

func quotaSnapshotRow(k modelCooldownKey, v modelCooldownEntry, now time.Time) map[string]any {
	return map[string]any{
		"model": k.ModelID, "reason": string(v.Reason), "scope": v.Scope,
		"until": v.Until.UTC().Format(time.RFC3339), "next_probe_at": v.NextProbeAt.UTC().Format(time.RFC3339),
		"occurred_at": v.OccurredAt.UTC().Format(time.RFC3339), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339),
		"seconds":            int(v.Until.Sub(now).Round(time.Second) / time.Second),
		"recovery_condition": v.RecoveryCondition, "evidence": v.Evidence,
		"persistent": false, "storage": "process_local", "state": "frozen",
	}
}
