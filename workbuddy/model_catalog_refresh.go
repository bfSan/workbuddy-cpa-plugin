// model_catalog_refresh.go runs a daily upstream model-catalog refresh after
// 23:00 local time.
//
// Motivation (2026-10-06): the per-model credit multiplier shown in the panel is
// sourced from the upstream model listing (see model_credits.go) and the
// upstream updates it on its own schedule, typically late in the day. Without a
// scheduled refresh the panel keeps showing yesterday's multipliers until
// somebody presses 刷新 by hand, and an operator sizing a request against a
// stale rate draws the wrong conclusion about headroom.
//
// Design:
//   - Runs on the existing checkinLoop (checkin.go) at catalogRefreshHour, the
//     same way checkin (09:00/21:00) and keepalive (22:00) do. Keeping it on
//     that loop means one wake-up scheduler, not two.
//   - Only the catalog is re-discovered; credits/plan are already refreshed by
//     the panel's own /credits path and by the check-in tick, so this does not
//     add billing-API traffic.
//   - The refresh is best-effort. A failed refresh keeps the previously loaded
//     catalog in place (refreshAllModelCatalogs restores the prior snapshot), so
//     an upstream outage at 23:00 cannot strip a working catalog.
package main

import (
	"encoding/json"
	"sync"
	"time"
)

// catalogRefreshHour is the hour (local time) at which the daily catalog refresh
// runs. It runs for the whole hour that starts here, matching how checkinHours
// and keepaliveHours behave.
//
// 23:00 is deliberately after the upstream's late-evening rate update, and after
// keepaliveHours (22:00) so the two do not contend for the same tick.
var catalogRefreshHour = 23

// catalogRefreshAuto gates the daily catalog refresh. Default true; configurable
// via plugin config key "model_refresh" (config.yaml line "model_refresh: false").
var (
	catalogRefreshAuto   = true
	catalogRefreshAutoMu sync.RWMutex
)

func catalogRefreshEnabled() bool {
	catalogRefreshAutoMu.RLock()
	defer catalogRefreshAutoMu.RUnlock()
	return catalogRefreshAuto
}

// catalogRefreshSummary is the last scheduled-run summary, exposed to the panel
// so an operator can tell whether the nightly refresh actually happened.
type catalogRefreshSummary struct {
	At      time.Time `json:"at"`
	OK      int       `json:"ok"`
	Failed  int       `json:"failed"`
	Detail  []string  `json:"detail,omitempty"`
	Trigger string    `json:"trigger"`
}

var (
	lastCatalogRefresh   catalogRefreshSummary
	lastCatalogRefreshMu sync.RWMutex
)

func recordCatalogRefresh(sum catalogRefreshSummary) {
	lastCatalogRefreshMu.Lock()
	lastCatalogRefresh = sum
	lastCatalogRefreshMu.Unlock()
}

func catalogRefreshStatus() catalogRefreshSummary {
	lastCatalogRefreshMu.RLock()
	defer lastCatalogRefreshMu.RUnlock()
	return lastCatalogRefresh
}

// refreshAllModelCatalogs re-discovers every account's catalog upstream. It is
// the scheduled counterpart of the panel's /models?refresh=1 and shares the same
// implementation, so both paths behave identically (including restoring the last
// good snapshot when upstream fails).
//
// trigger is recorded in the summary: "schedule" for the nightly run, "manual"
// for an operator-initiated one, which keeps the panel honest about why the
// numbers changed.
func refreshAllModelCatalogs(trigger string) catalogRefreshSummary {
	sum := catalogRefreshSummary{At: time.Now(), Trigger: trigger}
	runtime := activeModelRuntime.Load()
	if runtime == nil {
		sum.Failed = 1
		sum.Detail = append(sum.Detail, "model runtime not initialised")
		recordCatalogRefresh(sum)
		return sum
	}
	files, err := hostAuthList()
	if err != nil {
		sum.Failed = 1
		sum.Detail = append(sum.Detail, "list auths: "+err.Error())
		recordCatalogRefresh(sum)
		return sum
	}
	if len(files) == 0 {
		recordCatalogRefresh(sum)
		return sum
	}
	// Serial: refreshAllModelCatalogs is a low-frequency background job, and a
	// burst against the upstream model endpoint is exactly the kind of traffic
	// the credit gate exists to avoid. The panel's manual refresh keeps its own
	// concurrency; this path stays gentle.
	for _, f := range files {
		previous := runtime.snapshotForAuthID(f.ID)
		if _, err := runtime.refreshModelCatalogForAuth(f.ID, f.AuthIndex, "", previous); err != nil {
			sum.Failed++
			sum.Detail = append(sum.Detail, f.ID+": "+err.Error())
			continue
		}
		sum.OK++
	}
	recordCatalogRefresh(sum)
	return sum
}

// runScheduledCatalogRefresh is the nightly entry point, invoked from
// checkinLoop. It no-ops when the feature is switched off.
func runScheduledCatalogRefresh() {
	if !catalogRefreshEnabled() {
		return
	}
	refreshAllModelCatalogs("schedule")
}

// handleCatalogRefresh lets an operator trigger the same refresh on demand and
// read back when it last ran, so the panel can show a timestamp instead of
// implying the numbers are always current.
func handleCatalogRefresh(force bool) map[string]any {
	if force {
		return map[string]any{
			"summary": refreshAllModelCatalogs("manual"),
			"enabled": catalogRefreshEnabled(),
			"hour":    catalogRefreshHour,
		}
	}
	return map[string]any{
		"summary": catalogRefreshStatus(),
		"enabled": catalogRefreshEnabled(),
		"hour":    catalogRefreshHour,
	}
}

// catalogRefreshSummaryJSON renders the summary for the panel.
func catalogRefreshSummaryJSON(sum catalogRefreshSummary) string {
	raw, err := json.Marshal(sum)
	if err != nil {
		return "{}"
	}
	return string(raw)
}
