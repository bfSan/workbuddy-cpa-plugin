// checkin.go implements daily check-in for CN accounts: the manual
// handleManualCheckin endpoint, the 09:00 / 21:00 auto timer, and the
// per-account mutex that prevents duplicate check-ins from racing browser
// tabs. Global accounts are excluded — they use one-shot trial claims instead.
package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var (
	checkinLoopStop chan struct{}
	checkinLoopMu   sync.Mutex
)

func ensureCheckinLoop() {
	checkinLoopMu.Lock()
	defer checkinLoopMu.Unlock()
	if checkinLoopStop != nil {
		return // already running
	}
	checkinLoopStop = make(chan struct{})
	go checkinLoop(checkinLoopStop)
}

// Note: there is deliberately no stopCheckinScheduler. The plugin shutdown
// export is a no-op (see cliproxyPluginShutdown) because the host invokes it
// during its own runtime teardown, where touching Go sync primitives from the
// plugin's c-shared runtime caused SIGSEGV on every restart.

func scheduledInCurrentHour(now time.Time, hours []int) bool {
	for _, h := range hours {
		start := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !now.Before(start) && now.Before(start.Add(time.Hour)) {
			return true
		}
	}
	return false
}

func scheduledActionsFor(now time.Time) (runCheckin, runKeepalive, runCatalogRefresh bool) {
	return scheduledInCurrentHour(now, checkinHours),
		scheduledInCurrentHour(now, keepaliveHours),
		scheduledInCurrentHour(now, []int{catalogRefreshHour})
}

func nextCheckinTime(now time.Time) time.Time {
	var earliest time.Time
	// Consider every schedule so the timer wakes up for whichever fires first
	// (e.g. 21:00 checkin vs 22:00 keepalive → 21:00 wins, then 22:00 keepalive
	// fires on the next tick, then 23:00 catalog refresh).
	hours := append([]int{}, checkinHours...)
	hours = append(hours, keepaliveHours...)
	hours = append(hours, catalogRefreshHour)
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour) // slot already passed today → tomorrow
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

func checkinLoop(stop chan struct{}) {
	next := nextCheckinTime(time.Now())
	for {
		// Wake at most once per 30s for bounded retries. Preserve next across
		// retry ticks so 09:00/21:00 actions are not re-run on every wake.
		wait := time.Until(next)
		if wait > 30*time.Second {
			wait = 30 * time.Second
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-timer.C:
			now := time.Now()
			if !now.Before(next) {
				runCheckin, runKeepalive, runCatalogRefresh := scheduledActionsFor(now)
				if runCheckin {
					runAutoCheckin()
				}
				if runKeepalive {
					runTokenKeepalive()
				}
				if runCatalogRefresh {
					runScheduledCatalogRefresh()
				}
				next = nextCheckinTime(time.Now())
			}
			runDueCheckinRetries()
		}
	}
}

// runAutoCheckin is the scheduled lifecycle tick (09:00 / 21:00).
// CN: optional daily check-in, then reconcile (disable exhausted / reenable after credits).
// Global: no auto claim; reconciliation must never delete exhausted credentials.
//
// v0.6.31: per-account work runs concurrently (sem=4) — was serial, so N accounts
// meant 3N serial HTTP round-trips on the billing API. Matches the pattern used
// by buildDashboardEx and handleManualCheckin.
func runAutoCheckin() {
	checkinAutoMu.RLock()
	doCheckin := checkinAuto
	checkinAutoMu.RUnlock()
	// Lifecycle may still run when check-in is off (credit gate).
	if !doCheckin && !lifecycleEnabled() {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, f := range files {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			processAutoCheckinAccount(f, doCheckin)
		}()
	}
	wg.Wait()
}

// processAutoCheckinAccount handles one account's scheduled tick. Extracted so
// runAutoCheckin can fan out per-account work without duplicating logic.
func processAutoCheckinAccount(f pluginapi.HostAuthFileEntry, doCheckin bool) {
	if doCheckin {
		_ = checkinOneAccountFrom(f, "", "scheduled")
	}
	// Never reconcile under the per-account check-in lock: lifecycle helpers
	// acquire the same lock and preserve explicit manual disables.
	if lifecycleEnabled() {
		_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
	}
}

// handleManualCheckin serves POST /checkin.
//
// Single-account mode (body.auth_index set): one auth read and at most three
// upstream operations (status, claim, confirmation). No sleep in the RPC. The
// old three-phase classify/execute/summarize pipeline ran the same framework
// for one account as for thirty (4 RPC + 5 HTTP + lock-held re-reads), which
// routinely blew past the 30s management API timeout: the check-in succeeded
// upstream but the response never reached the panel.
//
// Batch mode (empty auth_index): fans out checkinOneAccount per account
// (sem=4), preserving input order and the {results, summary} response shape
// the panel depends on.
func handleManualCheckin(req pluginapi.ManagementRequest) map[string]any {
	return handleManualCheckinWithCallback(req, "")
}

func handleManualCheckinWithCallback(req pluginapi.ManagementRequest, callbackID string) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	single := authIndex != ""

	files, err := hostAuthList()
	if err != nil {
		return checkinListError()
	}
	var targets []pluginapi.HostAuthFileEntry
	for _, f := range files {
		if !single || f.AuthIndex == authIndex {
			targets = append(targets, f)
		}
	}
	if len(targets) == 0 {
		return map[string]any{"error": "no matching account"}
	}

	results := make([]map[string]any, len(targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, f := range targets {
		wg.Add(1)
		go func(i int, f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = checkinOneAccountWithCallback(f, callbackID)
		}(i, f)
	}
	wg.Wait()

	// Summary counters (field names are a panel contract — panel.html
	// checkinAll reads success/already/fail/skipped_global/eligible).
	successN, failN, alreadyN, globalN, eligibleN, pendingN, noneN := 0, 0, 0, 0, 0, 0, 0
	for _, out := range results {
		if out == nil {
			continue
		}
		if out["error"] != nil {
			failN++
			continue
		}
		reason, _ := out["reason"].(string)
		switch reason {
		case "already":
			alreadyN++
			continue
		case "global":
			globalN++
			continue
		case "pending_confirmation":
			pendingN++
			eligibleN++
			continue
		case "no_activity", "disabled":
			noneN++
			continue
		}
		eligibleN++
		if out["success"] == true {
			successN++
		} else {
			failN++
		}
	}
	return map[string]any{
		"results": results,
		"summary": map[string]any{
			"total":          len(targets),
			"eligible":       eligibleN,
			"success":        successN,
			"already":        alreadyN,
			"skipped_global": globalN,
			"fail":           failN,
			"pending":        pendingN,
			"no_activity":    noneN,
			"attempted":      eligibleN,
		},
	}
}

// Manual, scheduled and retry paths share the same confirmation contract.
func checkinOneAccount(f pluginapi.HostAuthFileEntry) map[string]any {
	return checkinOneAccountWithCallback(f, "")
}

func checkinOneAccountWithCallback(f pluginapi.HostAuthFileEntry, callbackID string) map[string]any {
	return checkinOneAccountFrom(f, callbackID, "manual")
}

func checkinOneAccountFrom(f pluginapi.HostAuthFileEntry, callbackID, source string) map[string]any {
	mu := checkinLockFor(f.AuthIndex)
	mu.Lock()
	defer mu.Unlock()
	if !checkinRuns.reserve(f.AuthIndex, time.Now()) {
		return map[string]any{"auth_index": f.AuthIndex, "success": false, "confirmed": false, "state": "failed", "reason": "capacity", "message": "签到任务容量已满，请等待现有任务完成"}
	}
	defer checkinRuns.release(f.AuthIndex)
	confirmOnly := checkinRuns.needsConfirmation(f.AuthIndex, time.Now())
	if source == "retry" {
		var due bool
		confirmOnly, due = checkinRuns.retryMode(f.AuthIndex, time.Now())
		if !due {
			return map[string]any{"auth_index": f.AuthIndex, "skipped": true, "reason": "cancelled", "success": false}
		}
	}
	sa, phys, err := hostAuthGetBundle(f.AuthIndex)
	if err != nil {
		return checkinRuns.rememberResult(f.AuthIndex, f.ID, source, checkinAttemptResult{State: "failed", Message: "无法读取账号凭据，未发起签到"})
	}
	if isGlobalDomain(sa.Auth.Domain) {
		return checkinRuns.rememberResult(f.AuthIndex, f.ID, source, checkinAttemptResult{State: "global", Message: "当前区域不支持该签到接口"})
	}
	// Background attempts never override a manual pause. Explicit manual
	// check-in is still permitted, but never toggles the credential's state.
	if source != "manual" && (f.Disabled || phys != nil && phys.Disabled) {
		return checkinRuns.rememberResult(f.AuthIndex, f.ID, source, checkinAttemptResult{State: "disabled", Message: "账号已禁用，跳过后台签到"})
	}
	result := confirmedCheckinAttempt(checkinProtocol{
		status: func() (*checkinSummary, error) { return fetchCheckinStatusWithCallback(sa, callbackID) },
		claim:  func() (map[string]any, error) { return performCheckinCallWithCallback(sa, callbackID) },
	}, confirmOnly)
	if result.Status != nil {
		mergeCheckinCache(f.ID, result.Status)
	}
	out := checkinRuns.rememberResult(f.AuthIndex, f.ID, source, result)
	out["nickname"] = sa.Account.Nickname
	return out
}

// Runs due work on the existing timer rather than spawning unbounded per-click
// goroutines. Every retry checks today's budget and current account state again.
func runDueCheckinRetries() {
	checkinAutoMu.RLock()
	auto := checkinAuto
	checkinAutoMu.RUnlock()
	due := checkinRuns.due(time.Now(), auto)
	if len(due) == 0 {
		return
	}
	files, err := hostAuthList()
	if err != nil {
		return
	}
	byIndex := make(map[string]pluginapi.HostAuthFileEntry, len(files))
	for _, f := range files {
		byIndex[f.AuthIndex] = f
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, idx := range due {
		f, ok := byIndex[idx]
		if !ok {
			checkinRuns.cancel(idx)
			continue
		}
		wg.Add(1)
		go func(f pluginapi.HostAuthFileEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out := checkinOneAccountFrom(f, "", "retry")
			if out["confirmed"] == true && lifecycleEnabled() {
				_, _ = reconcileOneAccount(f.AuthIndex, f.ID, true)
			}
		}(f)
	}
	wg.Wait()
}

// mergeCheckinCache stores the latest check-in snapshot while preserving the
// credits/plan fields of any previous cache entry (merge, not replace).
func mergeCheckinCache(authID string, ci *checkinSummary) {
	observed := time.Now()
	for {
		prev, exists := accountCache.Load(authID)
		entry := &accountCacheEntry{checkin: ci, checkinAt: observed}
		if exists {
			if cached, ok := prev.(*accountCacheEntry); ok {
				entry.credits, entry.plan = cached.credits, cached.plan
				// A check-in query does not refresh the age of the credit snapshot.
				entry.fetched = cached.fetched
			}
			if accountCache.CompareAndSwap(authID, prev, entry) {
				return
			}
		} else if _, loaded := accountCache.LoadOrStore(authID, entry); !loaded {
			return
		}
	}
}

func withCheckinLock(authIndex string, fn func()) {
	mu := checkinLockFor(authIndex)
	mu.Lock()
	defer mu.Unlock()
	fn()
}

func checkinLockFor(authIndex string) *sync.Mutex {
	v, _ := checkinLocks.LoadOrStore(authIndex, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// pruneCheckinLocks removes lock entries for auth indices that no longer
// exist in hostAuthList. Call after dashboard prune.
// Lock keys are auth_index (used for host RPC), so live map needs auth_index too.
func pruneCheckinLocks() {
	files, err := hostAuthList()
	if err != nil {
		return
	}
	live := make(map[string]struct{}, len(files))
	for _, f := range files {
		live[f.ID] = struct{}{}
		live[f.AuthIndex] = struct{}{} // checkinLockFor uses auth_index as key
	}
	checkinLocks.Range(func(key, _ any) bool {
		idx, _ := key.(string)
		if _, ok := live[idx]; !ok {
			checkinLocks.Delete(key)
		}
		return true
	})
}
