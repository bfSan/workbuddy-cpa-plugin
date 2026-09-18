// model_credits.go holds the per-model credit multiplier the upstream reports.
//
// pluginapi.ModelInfo has no cost/rate field, so the multiplier cannot be
// handed to CPA for billing or display. It stays inside the plugin: the panel
// shows it, and the estimation helpers use it to judge whether an account has
// enough headroom for a model before the request is spent.
//
// Precedence, matching the hub behaviour: upstream value > local override >
// nothing. A local override exists because upstream sometimes omits the field
// (or reports a stale value during a promotion), and the operator needs a way
// to pin it without editing code.
package main

import (
	"strconv"
	"strings"
	"sync"
)

const modelCreditsMaxEntries = 4096

var (
	creditsMu sync.RWMutex
	// upstreamCredits is what the source reported, keyed by model ID.
	upstreamCredits = make(map[string]string)
	// overrideCredits is the operator's pinned value, set from the panel.
	overrideCredits = make(map[string]string)
)

// recordModelCredits stores the multiplier a model source reported. An empty
// value clears any previously recorded upstream value so a source that stops
// reporting the field does not leave a stale number behind.
func recordModelCredits(modelID, credits string) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	creditsMu.Lock()
	defer creditsMu.Unlock()
	if credits == "" {
		delete(upstreamCredits, modelID)
		return
	}
	if len(upstreamCredits) >= modelCreditsMaxEntries && upstreamCredits[modelID] == "" {
		return
	}
	upstreamCredits[modelID] = credits
}

// setModelCreditsOverride pins a multiplier, or clears the override with an
// empty value so the upstream number takes over again.
func setModelCreditsOverride(modelID, credits string) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return
	}
	creditsMu.Lock()
	defer creditsMu.Unlock()
	if credits == "" {
		delete(overrideCredits, modelID)
		return
	}
	if len(overrideCredits) >= modelCreditsMaxEntries && overrideCredits[modelID] == "" {
		return
	}
	overrideCredits[modelID] = credits
}

// syncConfiguredCredits replaces the override table with the YAML
// `model_credits` block. Config is the persistent source of truth, so a
// reload wins over any panel-only pin — the same trade-off as the model list
// overlay, which is also process-local.
func syncConfiguredCredits(configured map[string]string) {
	creditsMu.Lock()
	defer creditsMu.Unlock()
	overrideCredits = make(map[string]string, len(configured))
	for id, value := range configured {
		if value == "" {
			continue
		}
		overrideCredits[id] = value
	}
}

// modelCredits resolves the effective multiplier and where it came from.
// Source is "override", "upstream", or "" when unknown.
func modelCredits(modelID string) (string, string) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", ""
	}
	creditsMu.RLock()
	defer creditsMu.RUnlock()
	if value, ok := overrideCredits[modelID]; ok && value != "" {
		return value, "override"
	}
	if value, ok := upstreamCredits[modelID]; ok && value != "" {
		return value, "upstream"
	}
	return "", ""
}

// parseModelCreditsRate extracts the numeric factor from an upstream string.
// Upstream is inconsistent: "x0.29", "x2.20 credits", "x0.00", "0.50x".
// Returns 0 for unknown or unparseable values, which callers must treat as
// "no estimate" rather than "free".
func parseModelCreditsRate(credits string) float64 {
	value := strings.TrimSpace(strings.ToLower(credits))
	if value == "" {
		return 0
	}
	value = strings.TrimSuffix(value, " credits")
	value = strings.TrimSuffix(value, "credit")
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "x")
	value = strings.TrimSuffix(value, "x")
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if strings.ContainsAny(value, " \t\r\n") {
		return 0
	}
	rate, err := strconv.ParseFloat(value, 64)
	if err != nil || rate < 0 {
		return 0
	}
	return rate
}

// modelCreditsField is the management API shape for one model's multiplier:
// the effective string, its numeric rate when parseable, and where it came
// from. A model with no known multiplier reports an empty string rather than
// a misleading zero.
func modelCreditsField(modelID string) map[string]any {
	value, source := modelCredits(modelID)
	if value == "" {
		return map[string]any{"value": "", "rate": 0, "source": ""}
	}
	return map[string]any{
		"value":  value,
		"rate":   parseModelCreditsRate(value),
		"source": source,
	}
}

// modelCreditsSnapshot lists every known multiplier with its source, sorted by
// model ID. Used by the panel and the management API.
func modelCreditsSnapshot() []map[string]any {
	creditsMu.RLock()
	ids := make([]string, 0, len(upstreamCredits)+len(overrideCredits))
	seen := make(map[string]struct{}, len(upstreamCredits)+len(overrideCredits))
	for id := range upstreamCredits {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range overrideCredits {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	creditsMu.RUnlock()
	sortStrings(ids)
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		value, source := modelCredits(id)
		if value == "" {
			continue
		}
		out = append(out, map[string]any{
			"id":      id,
			"credits": value,
			"rate":    parseModelCreditsRate(value),
			"source":  source,
		})
	}
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
