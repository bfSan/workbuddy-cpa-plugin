// cooldown.go implements per-(account, model) throttling.
//
// A 429 from one model says nothing about the account's ability to serve other
// models, so cooling the whole credential wastes the rest of its quota. This
// tracks failures at the (authID, modelID) pair so only the affected pair is
// reported as throttled.
//
// Confirmed shared-credit exhaustion uses an account-scoped entry in this same
// bounded table. Credential invalidation stays separate in lifecycle handling.
//
// State is process-local: it survives config reloads but not a CPA restart.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	// Default durations; kept short enough that a transient burst recovers on
	// its own without an operator intervening.
	modelCooldownRateLimit = 300 * time.Second
	modelCooldownUnknown   = 60 * time.Second

	// modelCooldownMaxEntries bounds the table so a hostile or buggy upstream
	// cannot grow it without limit.
	modelCooldownMaxEntries = 4096
)

type cooldownReason string

const (
	cooldownReasonRateLimit cooldownReason = "rate_limit"
	cooldownReasonUnknown   cooldownReason = "upstream_error"
)

type modelCooldownKey struct {
	AuthID  string
	ModelID string
}

type modelCooldownEntry struct {
	Reason            cooldownReason
	Scope             string
	Until             time.Time
	NextProbeAt       time.Time
	OccurredAt        time.Time
	UpdatedAt         time.Time
	RecoveryCondition string
	Evidence          string
}

var (
	cooldownMu     sync.Mutex
	cooldownTable  = make(map[modelCooldownKey]modelCooldownEntry)
	cooldownNowFn  = time.Now
	cooldownMaxTTL = modelCooldownRateLimit
)

// markModelCooldown records a throttled (account, model) pair. An empty model
// is ignored: without a model ID the entry would freeze the account, which is
// exactly what this table exists to avoid.
func markModelCooldown(authID, model string, reason cooldownReason) {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" || model == "" {
		return
	}
	ttl := modelCooldownRateLimit
	switch reason {
	case cooldownReasonRateLimit:
		ttl = modelCooldownRateLimit
	default:
		ttl = modelCooldownUnknown
	}
	markQuotaFreeze(authID, model, chatFailure{Reason: reason, Scope: quotaScopeModel, Evidence: "legacy_cooldown"}, cooldownNowFn().Add(ttl), "bounded_ttl_probe")
}

// modelCoolingUntil reports when the pair becomes usable again. The zero time
// means the pair is not cooling down.
func modelCoolingUntil(authID, model string) time.Time {
	entry, ok := quotaFreezeFor(authID, model)
	if !ok {
		return time.Time{}
	}
	return entry.Until
}

// modelIsCooling reports whether the pair should be skipped right now.
func modelIsCooling(authID, model string) bool {
	return !modelCoolingUntil(authID, model).IsZero()
}

// coolingModelSetFor returns the model IDs this account is cooling right now.
//
// One snapshot instead of a modelIsCooling call per catalog entry: that would
// take the lock once per model on every model.for_auth, and could also observe
// a different set partway through if a cooldown landed mid-loop. Expired
// entries are dropped on the way out, the same sweep modelCoolingUntil does.
func coolingModelSetFor(authID string) map[string]struct{} {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	var out map[string]struct{}
	for key, entry := range cooldownTable {
		if key.AuthID != authID || key.ModelID == "" {
			continue
		}
		if !entry.Until.After(now) {
			delete(cooldownTable, key)
			continue
		}
		if out == nil {
			out = make(map[string]struct{}, 4)
		}
		out[strings.TrimSpace(key.ModelID)] = struct{}{}
	}
	return out
}

// clearModelCooldown removes one pair, or every pair for the account when the
// model is empty. Returns how many entries were removed.
func clearModelCooldown(authID, model string) int {
	authID = strings.TrimSpace(authID)
	model = strings.TrimSpace(model)
	if authID == "" {
		return 0
	}
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	removed := 0
	if model != "" {
		key := modelCooldownKey{AuthID: authID, ModelID: model}
		if _, ok := cooldownTable[key]; ok {
			delete(cooldownTable, key)
			removed++
		}
		return removed
	}
	for k := range cooldownTable {
		if k.AuthID == authID {
			delete(cooldownTable, k)
			removed++
		}
	}
	return removed
}

// cooldownSnapshotFor lists the still-active pairs for one account.
func cooldownSnapshotFor(authID string) []map[string]any {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if k.AuthID != authID || !v.Until.After(now) {
			continue
		}
		out = append(out, quotaSnapshotRow(k, v, now))
	}
	cooldownMu.Unlock()
	sortCooldownSnapshot(out)
	return out
}

func sortCooldownSnapshot(rows []map[string]any) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, _ := rows[j-1]["model"].(string)
			b, _ := rows[j]["model"].(string)
			if b >= a {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// cooldownSweepLocked drops expired entries. Callers must hold cooldownMu.
func cooldownSweepLocked(now time.Time) {
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			delete(cooldownTable, k)
		}
	}
}

// recordUpstreamFailure routes an upstream error to the right cooling path.
// Only explicit shared-credit evidence may broaden the scope beyond the pair.
func recordUpstreamFailure(authID, model string, status int, body string) {
	recordUpstreamFailureWithHeaders(authID, model, status, body, nil)
}

// cooldownAccountCount reports how many pairs are cooling for one account.
// Used by the dashboard so the panel can show a badge without the full list.
func cooldownAccountCount(authID string) int {
	return len(cooldownSnapshotFor(authID))
}

// cooldownSnapshotAll lists every active pair across accounts. Used by the
// management endpoint so an operator can see the whole throttled surface.
func cooldownSnapshotAll() []map[string]any {
	now := cooldownNowFn()
	cooldownMu.Lock()
	out := make([]map[string]any, 0)
	for k, v := range cooldownTable {
		if !v.Until.After(now) {
			continue
		}
		row := quotaSnapshotRow(k, v, now)
		row["auth_id"] = k.AuthID
		out = append(out, row)
	}
	cooldownMu.Unlock()
	sortCooldownSnapshotByAuth(out)
	return out
}

func sortCooldownSnapshotByAuth(rows []map[string]any) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0; j-- {
			a, _ := rows[j-1]["auth_id"].(string)
			b, _ := rows[j]["auth_id"].(string)
			if b > a {
				break
			}
			if b < a {
				rows[j-1], rows[j] = rows[j], rows[j-1]
				continue
			}
			am, _ := rows[j-1]["model"].(string)
			bm, _ := rows[j]["model"].(string)
			if bm >= am {
				break
			}
			rows[j-1], rows[j] = rows[j], rows[j-1]
		}
	}
}

// cooldownModelCount reports how many accounts are throttled for one model.
// Used by the catalog view so the panel can flag a model that is degraded
// somewhere without pretending the throttle is global.
func cooldownModelCount(model string) int {
	model = strings.TrimSpace(model)
	if model == "" {
		return 0
	}
	now := cooldownNowFn()
	cooldownMu.Lock()
	defer cooldownMu.Unlock()
	n := 0
	for k, v := range cooldownTable {
		if k.ModelID == model && v.Until.After(now) {
			n++
		}
	}
	return n
}

// handleCooldownList reports every active pair, or just one account's pairs
// when auth_id is given. State lives in memory, so the response says so.
func handleCooldownList(req pluginapi.ManagementRequest) map[string]any {
	if id := strings.TrimSpace(req.Query.Get("auth_id")); id != "" {
		return map[string]any{
			"entries":    cooldownSnapshotFor(id),
			"count":      cooldownAccountCount(id),
			"persistent": false,
		}
	}
	entries := cooldownSnapshotAll()
	return map[string]any{
		"entries":    entries,
		"count":      len(entries),
		"persistent": false,
	}
}

// handleCooldownClear drops throttling for one pair (auth_id + model) or for
// every pair of one account (auth_id only). Refuses to clear everything at
// once — that would be a footgun reachable from a single API call.
func handleCooldownClear(req pluginapi.ManagementRequest) map[string]any {
	authID := strings.TrimSpace(req.Query.Get("auth_id"))
	model := strings.TrimSpace(req.Query.Get("model"))
	if authID == "" && len(req.Body) > 0 {
		var body struct {
			AuthID  string `json:"auth_id"`
			AuthIdx string `json:"auth_index"`
			Model   string `json:"model"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil {
			authID = strings.TrimSpace(body.AuthID)
			if authID == "" {
				authID = strings.TrimSpace(body.AuthIdx)
			}
			model = strings.TrimSpace(body.Model)
		}
	}
	if authID == "" {
		return map[string]any{"success": false, "error": "auth_id is required"}
	}
	removed := clearModelCooldown(authID, model)
	return map[string]any{
		"success":    true,
		"auth_id":    authID,
		"model":      model,
		"removed":    removed,
		"persistent": false,
	}
}
