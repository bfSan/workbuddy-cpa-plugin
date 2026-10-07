package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestAccountToggleRouteIsRegisteredAndWritesGuarded pins the manual enable /
// disable switch: the route must exist, be treated as a write (so the plugin's
// management-key gate applies), and reach the handler.
func TestAccountToggleRouteIsRegisteredAndWritesGuarded(t *testing.T) {
	// Declared routes are relative to the host's management prefix (CPA adds it),
	// while mutatingManagementPath is consulted with the fully prefixed path.
	if !mutatingManagementPath(loadedManagementBasePath() + "/plugins/" + providerName + "/accounts/disabled") {
		t.Error("POST /accounts/disabled must be classified as a write, or it bypasses the management key")
	}
	found := false
	for _, route := range managementRegistration().Routes {
		if route.Method == http.MethodPost && route.Path == "/plugins/"+providerName+"/accounts/disabled" {
			found = true
		}
	}
	if !found {
		t.Error("the panel needs a POST /accounts/disabled route to drive the button")
	}
}

// TestManualDisableIsDurable is the regression that matters most here: a manual
// disable must survive a restart.
//
// The state is persisted in disabled_reason = "manual" precisely so it reloads
// intact. Recording nothing (the first attempt) looked fine in the panel and
// then undid itself: on the next reconcile shouldReenableCN saw an empty reason,
// fell through to its legacy note path, found a readable balance and no
// dead-session marker, and re-enabled the account.
func TestManualDisableIsDurable(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{Nickname: "t"}, Auth: storedTokens{Domain: "www.codebuddy.cn"}}

	// Manual disable, as handleAccountSetDisabled writes it.
	disabledRaw, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 余5 已用1", map[string]any{
		"disabled_reason": disableReasonManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !parseDisabledFromAuthJSON(disabledRaw) {
		t.Fatal("a manual disable must actually disable the auth")
	}
	// Reload: the reason must come back, or the next reconcile revives it.
	if got := authFileDisabledReason(disabledRaw); got != disableReasonManual {
		t.Fatalf("disabled_reason after reload = %q, want %q", got, disableReasonManual)
	}
	if shouldReenableCN(true, &creditsSummary{TotalRemain: 5}, authFileDisabledReason(disabledRaw), "") {
		t.Fatal("a manually parked account must NOT be revived by automation, even with a healthy balance")
	}
	// Also with an unreadable balance: the manual intent still stands.
	if shouldReenableCN(true, nil, authFileDisabledReason(disabledRaw), "") {
		t.Fatal("a manually parked account must stay parked when the balance is unknown")
	}

	// Manual enable clears the field, so nothing stale is read later.
	enabledRaw, err := buildAuthFileJSON(sa, false, "CN · 余5 已用1", map[string]any{
		"disabled_reason": nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parseDisabledFromAuthJSON(enabledRaw) {
		t.Fatal("a manual enable must clear disabled")
	}
	if got := authFileDisabledReason(enabledRaw); got != "" {
		t.Fatalf("a manual enable must clear the stored reason (got %q)", got)
	}
}

// TestRewritePreservesUnmentionedFields pins the mechanism that made the manual
// park durable across the plugin's own rewrites.
//
// syncAuthNote runs on every reconcile and rebuilds the whole document from a
// fixed key list. Before buildAuthFileJSONFrom, anything the caller did not
// mention was dropped — so disabled_reason disappeared within one tick of being
// written, and the panel's 禁用 button could never stick.
func TestRewritePreservesUnmentionedFields(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{Nickname: "t"}, Auth: storedTokens{Domain: "www.codebuddy.cn"}}

	onDisk := []byte(`{"type":"workbuddy","provider":"workbuddy","disabled":true,` +
		`"note":"CN · 已禁用 · 余0 已用5","disabled_reason":"manual","future_field":"keep-me"}`)

	// A note-only rewrite (what syncAuthNote does) must not lose either field.
	rewritten, err := buildAuthFileJSONFrom(sa, true, "CN · 已禁用 · 余5 已用1", nil, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if got := authFileDisabledReason(rewritten); got != disableReasonManual {
		t.Fatalf("disabled_reason was dropped by a rewrite: got %q", got)
	}
	var doc map[string]any
	if err := json.Unmarshal(rewritten, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["future_field"] != "keep-me" {
		t.Errorf("an unknown key was dropped by a rewrite: got %v", doc["future_field"])
	}
	// The keys this function owns must still reflect its arguments.
	if doc["note"] != "CN · 已禁用 · 余5 已用1" {
		t.Errorf("note = %v, want the new value", doc["note"])
	}
	if doc["disabled"] != true {
		t.Errorf("disabled = %v, want true", doc["disabled"])
	}

	// An explicit null in extra removes the key, which is how 启用 clears it.
	cleared, err := buildAuthFileJSONFrom(sa, false, "CN · 余5 已用1", map[string]any{"disabled_reason": nil}, onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if got := authFileDisabledReason(cleared); got != "" {
		t.Fatalf("an explicit null must delete the key, got %q", got)
	}

	// With no base, behaviour is the historical one (fresh file).
	fresh, err := buildAuthFileJSON(sa, false, "CN · 余5 已用1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := authFileDisabledReason(fresh); got != "" {
		t.Fatalf("a fresh file must not invent a reason, got %q", got)
	}
}

// TestAccountToggleRequiresTARGET verifies the handler's argument handling
// without touching a real auth file: missing auth_index and missing disabled are
// both rejected before any I/O, so a malformed panel call cannot flip a random
// account.
func TestAccountToggleRequiresTarget(t *testing.T) {
	got := handleAccountSetDisabled(pluginapi.ManagementRequest{})
	if got["error"] != "auth_index is required" {
		t.Fatalf("empty request = %v, want an auth_index error", got)
	}

	body, err := json.Marshal(map[string]any{"auth_index": "idx"})
	if err != nil {
		t.Fatal(err)
	}
	got = handleAccountSetDisabled(pluginapi.ManagementRequest{Body: body})
	if got["error"] != "disabled is required" {
		t.Fatalf("missing disabled = %v, want a disabled error", got)
	}
}

// TestPanelCarriesAccountToggleButtons pins the UI wiring: both directions must
// be reachable from a card, and the switch must confirm before acting (parking a
// credential is disruptive enough to deserve a prompt).
func TestPanelCarriesAccountToggleButtons(t *testing.T) {
	html := string(servePanel(""))
	for _, needle := range []string{
		`data-action="disable"`,
		`data-action="enable"`,
		`/accounts/disabled`,
		"setAccountDisabled",
		"自动生命周期不会覆盖这个手工状态",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("panel is missing %q", needle)
		}
	}
	// The button must be the opposite of the CURRENT state, or an operator could
	// not tell which way it goes.
	if !strings.Contains(html, "const toggleBtn=disabled") {
		t.Error("the toggle must depend on the account's current disabled state")
	}
}
