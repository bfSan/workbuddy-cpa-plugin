// policy.go is the pure decision layer for credit-driven lifecycle actions:
// given an account's region and current credits, decide whether to disable
// (CN), delete (Global), re-enable (CN after check-in restores credits), or
// leave it alone. No I/O happens here — reconcileOneAccount consumes these
// decisions and applies them via the lifecycle.go authfile helpers.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// lifecycleAction is the policy decision for one account.
type lifecycleAction int

const (
	lifecycleNone lifecycleAction = iota
	lifecycleDisable
	lifecycleDelete
	lifecycleReenable
)

func (a lifecycleAction) String() string {
	switch a {
	case lifecycleDisable:
		return "disable"
	case lifecycleDelete:
		return "delete"
	case lifecycleReenable:
		return "reenable"
	default:
		return "none"
	}
}

// lifecycleAuto gates automatic disable/delete/reenable. Default true.
var (
	lifecycleAuto   = true
	lifecycleAutoMu sync.RWMutex
)

func lifecycleEnabled() bool {
	lifecycleAutoMu.RLock()
	defer lifecycleAutoMu.RUnlock()
	return lifecycleAuto
}

// shouldActOnCredits is true only when credits are *known* exhausted.
// nil / empty (no packages, no used) is unknown → false.
func shouldActOnCredits(cr *creditsSummary) bool {
	return isCreditsExhausted(cr)
}

// hardCreditMarkers are case-insensitive substrings in upstream error bodies.
var hardCreditMarkers = []string{
	"insufficient credit",
	"insufficient credits",
	"no credit",
	"no credits",
	"credit exhausted",
	"credits exhausted",
	"out of credit",
	"out of credits",
	"quota exceeded",
	"quota exhaust",
	"payment required",
	"积分不足",
	"额度不足",
	"余额不足",
	"积分用完",
	"额度用尽",
	"额度已用尽",
	"没有积分",
	"credit not enough",
	"not enough credit",
}

// isHardCreditError reports business "out of credits" style failures.
// 402 is treated as payment/credit. Pure 429 is not hard unless body has credit markers.
func isHardCreditError(status int, body string) bool {
	if status == httpStatusPaymentRequired {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range hardCreditMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	// Chinese markers may not lower-map usefully; also scan raw.
	for _, m := range hardCreditMarkers {
		if strings.Contains(body, m) {
			return true
		}
	}
	return false
}

const httpStatusPaymentRequired = 402

// isSoftRateLimit is pure throttling without hard-credit semantics.
func isSoftRateLimit(status int, body string) bool {
	if isHardCreditError(status, body) {
		return false
	}
	if status == 429 {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "throttl")
}

// lifecycleActionFor chooses disable/delete/none from region + credits.
// Does not consider reenable (that needs disabled flag).
//
// ★ Credits exhausted no longer disables (CN) or deletes (Global) the account.
// The catalog contains rate x0.00 (free) models, and those stay callable on a
// credential with 0 remaining credits — so "耗尽" does NOT mean the credential
// is useless. The old behaviour disabled the whole auth, which took every free
// model down with it: the catalog stayed listed but every request answered
// auth_unavailable, and the panel showed the account as disabled rather than as
// out of credit.
//
// The upstream is the authority on what a drained credential may still call: a
// refused paid model answers with a hard-credit error, which isHardCreditError
// already classifies for cooldown and for the panel's exhausted badge. Leaving
// the account enabled costs nothing, because CPA routes elsewhere when a
// credential genuinely cannot serve.
//
// Kept as a named function (rather than deleting the call site) so the region
// split and its rationale remain visible if this ever needs to come back.
func lifecycleActionFor(region string, cr *creditsSummary) lifecycleAction {
	if !shouldActOnCredits(cr) {
		return lifecycleNone
	}
	// Exhausted: leave the credential in place so free (rate 0) models keep
	// working; a paid model's own upstream error is the correct signal.
	return lifecycleNone
}

// Disable reasons are recorded in a dedicated auth-file field rather than being
// inferred from the note.
//
// The note cannot carry this reliably: syncAuthNote rebuilds it on every
// reconcile via displayNoteWithPrev, which keeps only the region / 已禁用 /
// credit segments (creditSegmentFromNote drops everything else). A reason
// appended to the note therefore survives at most until the next note refresh —
// exactly when the decision is made. The field, by contrast, is written through
// buildAuthFileJSON's extra map and never regenerated implicitly.
const (
	// disableReasonExhausted marks an account disabled because credits ran out.
	// That reason has since been retired (see lifecycleActionFor): rate-0 models
	// stay callable on a drained credential, so such an account is revived.
	disableReasonExhausted = "exhausted"
	// disableReasonSessionDead marks a credential whose offline session upstream
	// revoked. The credential itself is invalid, so it stays disabled until a
	// re-login replaces it; reviving it would put a known-bad credential back
	// into rotation.
	disableReasonSessionDead = "session_dead"
	// disableReasonManual marks an account parked by the operator through the
	// panel's 禁用 button. Automation must never undo a deliberate choice, so this
	// reason blocks re-enable outright; only the 启用 button (or a re-login)
	// clears it.
	//
	// This exists because the alternative — recording no reason — was actively
	// wrong: with no reason, shouldReenableCN falls through to its legacy path,
	// finds a readable balance and no dead-session marker, and re-enables the
	// account on the very next reconcile. The operator's disable would appear to
	// work and then silently undo itself.
	disableReasonManual = "manual"
)

// authFileDisabledReason extracts disabled_reason from a raw auth-file body.
// Empty means "unknown", which covers accounts disabled before the field
// existed.
func authFileDisabledReason(raw []byte) string {
	var doc struct {
		DisabledReason string `json:"disabled_reason"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return strings.TrimSpace(doc.DisabledReason)
}

// shouldReenableCN is true when a CN account is disabled and should be brought
// back.
//
// ★ It no longer requires positive credits. Exhausted credits stopped being a
// reason to disable an account (see lifecycleActionFor): rate-0 models stay
// callable on a drained credential, so a disabled account with 0 remaining is
// not a contradiction — it is a leftover from when exhaustion disabled it.
//
// Without this, the fix above would only stop NEW disables: every account
// already disabled by the old rule (e.g. an account whose credits ran out
// before the upgrade) would stay disabled forever, because the re-enable path
// demanded TotalRemain > 0 and the balance never moves while nothing can call
// it. That is a deadlock, not a policy.
//
// Re-enable is gated on the recorded reason, because not every disabled account
// may come back:
//
//   - disabled_reason == exhausted → re-enable; the reason no longer exists;
//   - disabled_reason == session_dead → keep disabled; re-login is the fix;
//   - disabled_reason == manual → keep disabled; the operator decided this, and
//     only the panel's 启用 button undoes it;
//   - no reason (pre-upgrade account) → fall back to the note text once, so an
//     account disabled for exhaustion under the old code is still rescued,
//     while one disabled for a dead session still is not.
func shouldReenableCN(disabled bool, cr *creditsSummary, reason, prevNote string) bool {
	if !disabled {
		return false
	}
	if reason == disableReasonSessionDead {
		// The credential is dead, not merely out of credit.
		return false
	}
	if reason == disableReasonManual {
		// The operator parked this account on purpose. Automation does not get to
		// override that; the 启用 button is the way back.
		return false
	}
	if reason == disableReasonExhausted {
		// The reason this account was parked no longer applies, so revive it even
		// if the balance cannot be read: nothing else is going to un-park it, and
		// the balance cannot move while the account is disabled.
		return true
	}
	// No recorded reason: an account disabled before disabled_reason existed.
	// The note is the only remaining evidence, so consult it here and nowhere
	// else — it is not durable enough to drive the decision afterwards.
	if isSessionDeadNote(prevNote) {
		return false
	}
	if cr == nil {
		// Unknown balance and unknown reason: cannot tell a drained account from a
		// broken one, and a wrong re-enable would put a credit-refused credential
		// back into rotation.
		return noteSaysExhausted(prevNote)
	}
	return true
}

// creditExhaustedNoteMarker is the segment disableAuth appended when it disabled
// an account for out-of-credit, under the pre-disabled_reason scheme. Kept so
// accounts parked by that older code can still be recognised once.
const creditExhaustedNoteMarker = "耗尽"

// noteSaysExhausted reports whether an auth note records a credit disable.
func noteSaysExhausted(note string) bool {
	return strings.Contains(note, creditExhaustedNoteMarker)
}

// isSessionDeadNote reports whether an auth note marks a revoked offline
// session (keepalive.go sets "Session dead (12153): re-login required").
// Matched case-insensitively so both spellings of the marker work.
func isSessionDeadNote(note string) bool {
	lower := strings.ToLower(note)
	return strings.Contains(lower, "session dead") || strings.Contains(note, "12153")
}

// displayNote builds a one-line note for CPAMP Auth cards.
//
// cr == nil means "credits unknown right now" (startup, a lazy panel refresh,
// or a failed billing call). displayNote cannot read the disk, so it falls back
// to the placeholder; callers that can see the stored note should prefer
// displayNoteWithPrev so a reload never regresses a live card.
func displayNote(sa *storedAuth, cr *creditsSummary, disabled bool) string {
	return displayNoteWithPrev(sa, cr, disabled, "")
}

// creditSegmentFromNote extracts the credit segment from an existing auth note,
// dropping the region / 已禁用 head. Returns "" when the note carries no usable
// credit information, so callers never resurrect the "积分未知" placeholder.
func creditSegmentFromNote(note string) string {
	segments := make([]string, 0, 4)
	for _, part := range strings.Split(note, " · ") {
		part = strings.TrimSpace(part)
		switch part {
		case "", "CN", "Global", "已禁用":
			continue
		}
		segments = append(segments, part)
	}
	seg := strings.Join(segments, " · ")
	if seg == "" || strings.HasPrefix(seg, "积分未知") {
		return ""
	}
	return seg
}

// rawNoteFromJSON returns the stored note verbatim, so lifecycle can tell WHY an
// account is disabled (耗尽 vs a dead session). noteCreditsFromJSON deliberately
// strips the region/已禁用 head and any non-credit segment, which would erase the
// SESSION-DEAD marker this needs.
func rawNoteFromJSON(raw []byte) string {
	var doc struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return doc.Note
}

// noteCreditsFromJSON extracts the credit segment from a raw auth-file body.
func noteCreditsFromJSON(raw []byte) string {
	var doc struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return ""
	}
	return creditSegmentFromNote(doc.Note)
}

// displayNoteWithPrev is displayNote plus a previously known credit segment.
// prev is ignored whenever cr carries fresh data or holds no usable value.
func displayNoteWithPrev(sa *storedAuth, cr *creditsSummary, disabled bool, prev string) string {
	region := strings.ToUpper(accountRegion(sa))
	if region == "CN" {
		region = "CN"
	} else {
		region = "Global"
	}
	parts := []string{region}
	if disabled {
		parts = append(parts, "已禁用")
	}
	switch {
	case cr == nil:
		if prev = creditSegmentFromNote(prev); prev != "" {
			parts = append(parts, prev)
		} else {
			parts = append(parts, "积分未知")
		}
	case isCreditsExhausted(cr):
		parts = append(parts, fmt.Sprintf("耗尽 · 余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
	default:
		// Show remain as primary (what you can still spend). Used is real cycle spend.
		// Size (capacity) grows with check-in packs — do not treat size↑ as usage↓.
		if cr.TotalSize > 0 {
			parts = append(parts, fmt.Sprintf("余%d 已用%d 池%d", cr.TotalRemain, cr.TotalUsed, cr.TotalSize))
		} else {
			parts = append(parts, fmt.Sprintf("余%d 已用%d", cr.TotalRemain, cr.TotalUsed))
		}
	}
	note := strings.Join(parts, " · ")
	if len(note) > 80 {
		note = note[:77] + "..."
	}
	return note
}

// labelForAuth adds [CN]/[Global] for host labels.
func labelForAuth(sa *storedAuth) string {
	base := "WorkBuddy"
	if sa != nil && strings.TrimSpace(sa.Account.Nickname) != "" {
		base = strings.TrimSpace(sa.Account.Nickname)
	}
	tag := "CN"
	if accountRegion(sa) == "global" {
		tag = "Global"
	}
	return base + " [" + tag + "]"
}
