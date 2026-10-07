package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestCatalogRefreshHourIsAfterKeepalive pins the schedule decision: the nightly
// multiplier refresh must run after the 22:00 keepalive tick so the two do not
// contend, and it must be the last slot of the day.
func TestCatalogRefreshHourIsAfterKeepalive(t *testing.T) {
	if catalogRefreshHour <= keepaliveHours[0] {
		t.Fatalf("catalogRefreshHour=%d must be later than keepalive %d so the two ticks do not collide",
			catalogRefreshHour, keepaliveHours[0])
	}
	for _, h := range checkinHours {
		if catalogRefreshHour == h {
			t.Fatalf("catalog refresh hour %d collides with a check-in hour", h)
		}
	}
	if catalogRefreshHour >= 24 || catalogRefreshHour < 0 {
		t.Fatalf("catalogRefreshHour=%d is not a valid hour", catalogRefreshHour)
	}
}

// TestCatalogRefreshToggleDefaultsOnAndIsConfigurable documents the on/off knob
// and that flipping it does not disturb the neighbouring toggles.
func TestCatalogRefreshToggleDefaultsOnAndIsConfigurable(t *testing.T) {
	original := catalogRefreshEnabled()
	t.Cleanup(func() {
		catalogRefreshAutoMu.Lock()
		catalogRefreshAuto = original
		catalogRefreshAutoMu.Unlock()
	})

	catalogRefreshAutoMu.Lock()
	catalogRefreshAuto = false
	catalogRefreshAutoMu.Unlock()
	if catalogRefreshEnabled() {
		t.Fatal("model_refresh: false must disable the scheduled refresh")
	}
	// Disabling the catalog refresh must not touch the other schedules.
	if !keepaliveEnabled() {
		t.Fatal("turning off the catalog refresh must not disable token keepalive")
	}

	catalogRefreshAutoMu.Lock()
	catalogRefreshAuto = true
	catalogRefreshAutoMu.Unlock()
	if !catalogRefreshEnabled() {
		t.Fatal("model_refresh should default to enabled")
	}
}

// TestScheduledCatalogRefreshRecordsSummary covers the summary the panel reads:
// with no model runtime installed the run must record an explicit failure rather
// than silently doing nothing (an operator needs to see that the nightly job ran
// and failed, not wonder whether it ran at all).
func TestScheduledCatalogRefreshRecordsSummary(t *testing.T) {
	previous := activeModelRuntime.Swap(nil)
	t.Cleanup(func() { activeModelRuntime.Store(previous) })

	recordCatalogRefresh(catalogRefreshSummary{})
	runScheduledCatalogRefresh()

	got := catalogRefreshStatus()
	if got.Trigger != "schedule" {
		t.Fatalf("trigger = %q, want schedule", got.Trigger)
	}
	if got.At.IsZero() {
		t.Fatal("summary must record when it ran")
	}
	if got.Failed == 0 {
		t.Fatal("a missing model runtime must be reported as a failure, not as a silent no-op")
	}
}

// TestCatalogRefreshStatusHandlerShape pins the management payload the panel
// consumes, including the schedule it advertises.
func TestCatalogRefreshStatusHandlerShape(t *testing.T) {
	got := handleCatalogRefresh(false)
	if _, ok := got["summary"]; !ok {
		t.Error("status payload must carry the last-run summary")
	}
	if got["enabled"] != catalogRefreshEnabled() {
		t.Error("status payload must report the on/off state")
	}
	if got["hour"] != catalogRefreshHour {
		t.Errorf("hour = %v, want %d", got["hour"], catalogRefreshHour)
	}
}

// TestPanelKicksCatalogDiscoveryOnColdLoad pins the first-load behaviour: the
// panel must not require the operator to press 刷新 before the catalog is
// discovered. The banner alone used to be the whole story, so a freshly started
// CPA sat on 模型目录尚未初始化 indefinitely.
func TestPanelKicksCatalogDiscoveryOnColdLoad(t *testing.T) {
	html := string(servePanel(""))
	for _, needle := range []string{
		"kickCatalogDiscovery",
		"catalogKickStarted",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("panel is missing %q; the catalog would stay cold until a manual refresh", needle)
		}
	}
	// It must be gated on a cold state, not fired on every load.
	if !strings.Contains(html, `ms.state!=="ready"&&ms.state!=="stale"`) {
		t.Error("discovery must only be kicked when the catalog is neither ready nor stale")
	}
	// And it must be one-shot, or every render would fan out a forced refresh.
	if !strings.Contains(html, "if(catalogKickStarted) return;") {
		t.Error("kickCatalogDiscovery must be one-shot")
	}
}

// TestConfigDeclaresModelRefresh guards the plugin config surface: without the
// field registered, an operator cannot turn the nightly refresh off.
func TestConfigDeclaresModelRefresh(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model_refresh"`) {
		t.Error("main.go must declare the model_refresh config field")
	}
	// The scalar type whitelist in usage_config.go must accept it too, or the
	// value is parsed and then rejected as an unknown key type.
	usage, err := os.ReadFile("usage_config.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(usage), `"checkin_auto", "lifecycle_auto", "token_keepalive", "model_refresh"`) {
		t.Error("usage_config.go must accept model_refresh as a boolean scalar")
	}
	if !strings.Contains(string(usage), `nextCatalogRefreshAuto = enabledConfigValue(value)`) {
		t.Error("usage_config.go must apply the model_refresh value")
	}
}

// TestCatalogRefreshIsGentle guards against the scheduled path fanning out the
// way the credit-gated upstream traffic must not: it iterates serially and
// consults every credential, so a burst cannot happen at 23:00.
func TestCatalogRefreshIsGentle(t *testing.T) {
	raw, err := os.ReadFile("model_catalog_refresh.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	if strings.Contains(src, "go func(") {
		t.Error("the scheduled catalog refresh must not spawn concurrent upstream calls")
	}
	// A status query must never trigger a refresh by itself.
	if !strings.Contains(src, "func handleCatalogRefresh(force bool)") {
		t.Error("handleCatalogRefresh must distinguish a read from a forced run")
	}
	_ = time.Now
}

// TestCreditExhaustionNeverReachesDisableOrDelete walks the whole decision chain
// that starts when upstream reports a hard credit error — which is exactly what
// happens when a request for a PAID model hits a drained credential.
//
// The chain is: reconcileAfterExecutorError / reconcileByUID gate on
// isHardCreditError, then reconcileOneAccount fetches credits and asks
// lifecycleActionFor what to do. That last step must answer "none", because the
// catalog still serves rate-0 models on a drained credential. If it ever answers
// disable (CN) or delete (Global) again, every free model on that account stops
// working and the panel reports the account as disabled rather than out of
// credit — the regression this pins.
func TestCreditExhaustionNeverReachesDisableOrDelete(t *testing.T) {
	drained := &creditsSummary{TotalRemain: 0, TotalUsed: 5646, TotalSize: 5646}
	if !isHardCreditError(402, "") {
		t.Fatal("402 must classify as hard credit, or the chain below is never entered")
	}
	for _, region := range []string{"cn", "global", ""} {
		action := lifecycleActionFor(region, drained)
		switch action {
		case lifecycleDisable:
			t.Fatalf("region %q: drained credits must not disable the account; rate-0 models would stop", region)
		case lifecycleDelete:
			t.Fatalf("region %q: drained credits must not delete the account", region)
		}
		if action != lifecycleNone {
			t.Fatalf("region %q: action = %v, want none", region, action)
		}
	}
}

// TestPanelReportsExhaustedWithoutDisabling pairs the two operator-visible facts:
// the account keeps serving (action none) while the panel still says 耗尽, so an
// operator can tell "out of credit" apart from "account broken".
func TestPanelReportsExhaustedWithoutDisabling(t *testing.T) {
	drained := &creditsSummary{TotalRemain: 0, TotalUsed: 5646, TotalSize: 5646}
	if !isCreditsExhausted(drained) {
		t.Fatal("a drained credential must still be reported as exhausted")
	}
	if got := lifecycleActionFor("cn", drained); got != lifecycleNone {
		t.Fatalf("action = %v; exhausted must not mean disabled", got)
	}
	// The note must say 耗尽 rather than 已禁用, or the panel contradicts itself.
	note := displayNote(&storedAuth{}, drained, false)
	if !strings.Contains(note, "耗尽") {
		t.Errorf("note = %q, want it to mention 耗尽", note)
	}
	if strings.Contains(note, "已禁用") {
		t.Errorf("note = %q must not claim the account is disabled", note)
	}
}
