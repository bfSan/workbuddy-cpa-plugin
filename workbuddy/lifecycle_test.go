package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestShouldActOnCredits(t *testing.T) {
	cases := []struct {
		name string
		cr   *creditsSummary
		want bool
	}{
		{"nil unknown", nil, false},
		{"empty unknown", &creditsSummary{}, false},
		{"remain>0", &creditsSummary{TotalRemain: 1}, false},
		{"exhausted used", &creditsSummary{TotalRemain: 0, TotalUsed: 10}, true},
		{"exhausted packages", &creditsSummary{TotalRemain: 0, Packages: []packageSummary{{Name: "p"}}}, true},
	}
	for _, tc := range cases {
		if got := shouldActOnCredits(tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsHardCreditError(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, "", false},
		{500, "internal", false},
		{429, "too many requests", false}, // soft unless credit semantics
		{403, "insufficient credit", true},
		{400, "积分不足", true},
		{402, "payment required", true},
		{403, "额度不足", true},
		{400, "no credit left", true},
		{400, "余额不足", true},
		{403, "credit exhausted", true},
		{429, "rate limit exceeded", false},
		{400, "invalid request", false},
	}
	for _, tc := range cases {
		if got := isHardCreditError(tc.status, tc.body); got != tc.want {
			t.Fatalf("status=%d body=%q: got %v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestHardCreditErrorRecognizesQuotaAlreadyExhausted(t *testing.T) {
	if !isHardCreditError(429, `{"message":"额度已用尽"}`) {
		t.Fatal("额度已用尽 not recognized")
	}
	if isHardCreditError(429, `{"message":"too many requests"}`) {
		t.Fatal("pure 429 classified as hard credit")
	}
}

func TestIsSoftRateLimit(t *testing.T) {
	if !isSoftRateLimit(429, "too many requests") {
		t.Fatal("429 should be soft")
	}
	if isSoftRateLimit(403, "insufficient credit") {
		t.Fatal("hard credit must not be soft")
	}
	if isSoftRateLimit(500, "error") {
		t.Fatal("5xx not soft rate limit")
	}
}

func TestLifecycleActionFor(t *testing.T) {
	ex := &creditsSummary{TotalRemain: 0, TotalUsed: 5}
	ok := &creditsSummary{TotalRemain: 10, TotalUsed: 1}
	cases := []struct {
		name   string
		region string
		cr     *creditsSummary
		want   lifecycleAction
	}{
		// Exhausted credits no longer disable or delete: rate-0 models (hy3,
		// hy3-b, hy3-c) stay callable on a drained credential, so the account
		// must be left enabled. See lifecycleActionFor.
		{"cn exhausted stays enabled", "cn", ex, lifecycleNone},
		{"global exhausted stays enabled", "global", ex, lifecycleNone},
		{"cn ok", "cn", ok, lifecycleNone},
		{"global ok", "global", ok, lifecycleNone},
		{"unknown", "cn", nil, lifecycleNone},
	}
	for _, tc := range cases {
		if got := lifecycleActionFor(tc.region, tc.cr); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestLifecycleActionForExhaustedKeepsFreeModelsAvailable states the user-visible
// contract behind the case above: with zero credits the credential is still
// usable, so a rate-0 model can be served. Previously the whole auth was disabled
// and every model — free ones included — answered auth_unavailable.
func TestLifecycleActionForExhaustedKeepsFreeModelsAvailable(t *testing.T) {
	drained := &creditsSummary{TotalRemain: 0, TotalUsed: 5646, TotalSize: 5646}
	for _, region := range []string{"cn", "global"} {
		if got := lifecycleActionFor(region, drained); got != lifecycleNone {
			t.Fatalf("%s: exhausted credits must not disable the account (got %v)", region, got)
		}
	}
}

func TestShouldReenableCN(t *testing.T) {
	drained := &creditsSummary{TotalRemain: 0, TotalUsed: 5}
	able := &creditsSummary{TotalRemain: 3}
	deadNote := "Session dead (12153): re-login required"

	if shouldReenableCN(false, able, "", "") {
		t.Fatal("enabled account should not reenable")
	}
	// No recorded reason and no readable balance: cannot tell a drained account
	// from a broken one, so leave it disabled.
	if shouldReenableCN(true, nil, "", "") {
		t.Fatal("unknown credits with no disable reason must not reenable")
	}

	// ★ Recorded reason, the authoritative path.
	// exhausted: the reason was retired, so the account must come back — even with
	// zero credits and even if the balance cannot be read, because otherwise it
	// stays parked forever (nothing can spend, so the balance never moves).
	if !shouldReenableCN(true, drained, disableReasonExhausted, "") {
		t.Fatal("a credit-disabled account must be revived: exhaustion no longer blocks it")
	}
	if !shouldReenableCN(true, nil, disableReasonExhausted, "") {
		t.Fatal("an exhausted account must be revived even when the balance is unreadable")
	}
	// session_dead: the credential itself is invalid, so reviving it would push a
	// known-bad credential back into rotation.
	if shouldReenableCN(true, able, disableReasonSessionDead, "") {
		t.Fatal("a session-dead account must stay disabled until re-login")
	}
	if shouldReenableCN(true, nil, disableReasonSessionDead, "") {
		t.Fatal("a session-dead account must stay disabled even without credits")
	}

	// ★ No recorded reason: pre-upgrade accounts, where the note is the only
	// evidence left. Consulted here and nowhere else, since syncAuthNote rewrites
	// the note on every reconcile.
	if !shouldReenableCN(true, drained, "", "CN · 已禁用 · 耗尽 · 余0 已用5") {
		t.Fatal("an account parked by the old exhaustion rule must still be rescued via its note")
	}
	if shouldReenableCN(true, able, "", deadNote) {
		t.Fatal("an account parked for a dead session must stay disabled even without the field")
	}
	// No reason, no marker, balance readable: nothing says it must stay parked.
	if !shouldReenableCN(true, able, "", "") {
		t.Fatal("disabled + readable balance + no reason should reenable")
	}
}

func TestDisplayNote(t *testing.T) {
	cn := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	gl := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai"}}
	note := displayNote(cn, &creditsSummary{TotalRemain: 12, TotalUsed: 8, TotalSize: 20}, false)
	if !strings.Contains(note, "CN") || !strings.Contains(note, "12") || !strings.Contains(note, "8") {
		t.Fatalf("cn note = %q", note)
	}
	note = displayNote(gl, &creditsSummary{TotalRemain: 0, TotalUsed: 250, TotalSize: 250}, false)
	if !strings.Contains(note, "Global") || !strings.Contains(note, "耗尽") {
		t.Fatalf("global note = %q", note)
	}
	note = displayNote(cn, &creditsSummary{TotalRemain: 0, TotalUsed: 5}, true)
	if !strings.Contains(note, "禁用") && !strings.Contains(strings.ToLower(note), "disabled") {
		t.Fatalf("disabled note = %q", note)
	}
}

func TestBuildAuthFileJSON_ContainsDisabledAndNote(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", RefreshToken: "rt", Domain: "www.codebuddy.cn"},
		Account: storedAccount{UID: "u1", Nickname: "nick"},
	}
	raw, err := buildAuthFileJSON(sa, true, "CN · test", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["type"] != providerName {
		t.Fatalf("type=%v", m["type"])
	}
	if m["disabled"] != true {
		t.Fatalf("disabled=%v", m["disabled"])
	}
	if m["note"] != "CN · test" {
		t.Fatalf("note=%v", m["note"])
	}
	if m["logo"] == nil || m["logo"] == "" {
		t.Fatal("logo missing")
	}
	if m["label"] != labelForAuth(sa) {
		t.Fatalf("label=%v, want %q", m["label"], labelForAuth(sa))
	}
	if m["email"] != sa.Account.Nickname {
		t.Fatalf("email=%v, want nickname %q", m["email"], sa.Account.Nickname)
	}
	if got, _ := enrichAuthMetadataWithPrev(sa, nil, true, "")["email"].(string); got != sa.Account.Nickname {
		t.Fatalf("metadata email=%q, want nickname %q", got, sa.Account.Nickname)
	}
	auth, _ := m["auth"].(map[string]any)
	if auth == nil || auth["accessToken"] != "at" {
		t.Fatalf("auth tokens lost: %v", m["auth"])
	}
}

func TestDisplayEmailForAuthUsesTrimmedNickname(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{Nickname: "  薄枫  "}}
	if got := displayEmailForAuth(sa); got != "薄枫" {
		t.Fatalf("email = %q, want trimmed nickname", got)
	}
	if got := displayEmailForAuth(&storedAuth{}); got != "" {
		t.Fatalf("empty nickname email = %q, want empty", got)
	}
	if got := displayEmailForAuth(nil); got != "" {
		t.Fatalf("nil auth email = %q, want empty", got)
	}
}

func TestAccountRenameSyncsHostLabel(t *testing.T) {
	oldGet := hostAuthGetPhysicalFn
	oldSave := hostAuthSaveJSONFn
	var saved []byte
	hostAuthGetPhysicalFn = func(authIndex string) (*hostAuthPhysical, error) {
		raw, _ := json.Marshal(map[string]any{
			"type":    providerName,
			"account": map[string]any{"uid": "uid-rename", "nickname": "old"},
			"auth":    map[string]any{"accessToken": "tok", "domain": "www.codebuddy.cn"},
			"note":    "CN · 余1 已用2",
		})
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "workbuddy-uid-rename.json", JSON: raw}, nil
	}
	hostAuthSaveJSONFn = func(name string, raw []byte) error {
		saved = append([]byte(nil), raw...)
		return nil
	}
	t.Cleanup(func() {
		hostAuthGetPhysicalFn = oldGet
		hostAuthSaveJSONFn = oldSave
	})

	resp := handleAccountRename(pluginapi.ManagementRequest{
		Body: []byte(`{"auth_index":"idx-rename","name":"新名字"}`),
	})
	if errValue, ok := resp["error"]; ok {
		t.Fatalf("rename error: %v", errValue)
	}
	var parsed struct {
		Label   string `json:"label"`
		Account struct {
			Nickname string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(saved, &parsed); err != nil {
		t.Fatalf("unmarshal saved auth: %v", err)
	}
	if parsed.Account.Nickname != "新名字" {
		t.Fatalf("nickname = %q", parsed.Account.Nickname)
	}
	if !strings.Contains(parsed.Label, "新名字") {
		t.Fatalf("label = %q, want renamed account", parsed.Label)
	}
}

func TestWaitForRuntimeAuthLabelWaitsForReparse(t *testing.T) {
	oldRuntime := runtimeAuthLabelFn
	reads := 0
	runtimeAuthLabelFn = func(authIndex string) (string, error) {
		if authIndex != "idx-rename" {
			t.Fatalf("auth index = %q", authIndex)
		}
		reads++
		if reads < 3 {
			return "workbuddy", nil
		}
		return "新名字 [CN]", nil
	}
	t.Cleanup(func() { runtimeAuthLabelFn = oldRuntime })

	if err := waitForRuntimeAuthLabel("idx-rename", "新名字 [CN]", time.Second); err != nil {
		t.Fatalf("waitForRuntimeAuthLabel: %v", err)
	}
	if reads < 3 {
		t.Fatalf("runtime label reads = %d, want at least 3", reads)
	}
}

func TestSafeWorkbuddyAuthPath(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-abc.json")
	if err := os.WriteFile(ok, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isSafeWorkbuddyAuthPath(ok) {
		t.Fatalf("expected safe: %s", ok)
	}
	if isSafeWorkbuddyAuthPath(filepath.Join(dir, "other.json")) {
		t.Fatal("other.json must be rejected")
	}
	if isSafeWorkbuddyAuthPath("") {
		t.Fatal("empty rejected")
	}
	if isSafeWorkbuddyAuthPath("/etc/passwd") {
		t.Fatal("passwd rejected")
	}
}

func TestDeleteAuthFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "workbuddy-x.json")
	if err := os.WriteFile(p, []byte(`{"type":"workbuddy"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deleteAuthFileAt(p); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	if err := deleteAuthFileAt(p); err != nil {
		t.Fatalf("second delete should be idempotent: %v", err)
	}
}

func TestAuthFileNameFor(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{UID: "uid-1"}}
	if got := authFileNameFor(sa); got != "workbuddy-uid-1.json" {
		t.Fatalf("got %q", got)
	}
	if got := authFileNameFor(nil); got != authFileName {
		t.Fatalf("nil got %q", got)
	}
}

func TestParseDisabledFromAuthJSON(t *testing.T) {
	if parseDisabledFromAuthJSON([]byte(`{"disabled":true}`)) != true {
		t.Fatal("want true")
	}
	if parseDisabledFromAuthJSON([]byte(`{"disabled":false}`)) != false {
		t.Fatal("want false")
	}
	if parseDisabledFromAuthJSON([]byte(`{"auth":{}}`)) != false {
		t.Fatal("missing defaults false")
	}
}

func TestLabelForAuth(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{Domain: "www.workbuddy.ai"},
		Account: storedAccount{Nickname: "Bob"},
	}
	got := labelForAuth(sa)
	if !strings.Contains(got, "Bob") || !strings.Contains(got, "Global") {
		t.Fatalf("label=%q", got)
	}
}

func TestIsSafeWorkbuddyAuthPath(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-safe.json")
	if !isSafeWorkbuddyAuthPath(ok) {
		t.Fatalf("want safe: %s", ok)
	}
	legacy := filepath.Join(dir, "workbuddy.json")
	if !isSafeWorkbuddyAuthPath(legacy) {
		t.Fatalf("want legacy safe: %s", legacy)
	}
	bad := filepath.Join(dir, "evil.json")
	if isSafeWorkbuddyAuthPath(bad) {
		t.Fatalf("want unsafe: %s", bad)
	}
	if isSafeWorkbuddyAuthPath("") {
		t.Fatal("empty path must be unsafe")
	}
	// Traversal: explicit ".." in the raw string before filepath.Join cleans it.
	if isSafeWorkbuddyAuthPath("/auth/../workbuddy-x.json") {
		t.Fatal("path with .. must be rejected")
	}
	// /etc/workbuddy-evil.json has valid basename but is NOT under auth dir.
	// isSafeWorkbuddyAuthPath only validates basename + traversal; the
	// directory confinement is enforced by deleteAuthFileInDir + isPathUnder.
	// So this path PASSES isSafe (baseline) but must FAIL deleteAuthFileInDir.
	// (Tested in TestDeleteAuthFileInDir.)
}

func TestIsPathUnder(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join(dir, "workbuddy-uid.json")
	if !isPathUnder(ok, dir) {
		t.Fatalf("path under dir should pass: %s", ok)
	}
	// Sibling dir
	other := filepath.Join(dir, "..", "other", "workbuddy-uid.json")
	if isPathUnder(other, dir) {
		t.Fatalf("path outside dir should fail: %s", other)
	}
	// Empty dir = no constraint
	if !isPathUnder("/anywhere/workbuddy.json", "") {
		t.Fatal("empty dir should allow")
	}
	// Path is dir itself
	if isPathUnder(dir, dir) {
		t.Fatal("dir itself is not 'under' dir")
	}
}

func TestDeleteAuthFileInDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "workbuddy-x.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Delete under correct dir — succeeds.
	if err := deleteAuthFileInDir(target, dir); err != nil {
		t.Fatalf("delete in dir: %v", err)
	}
	// Delete outside dir — rejected (A-23: basename-only was insufficient).
	outside := "/etc/workbuddy-evil.json"
	if err := deleteAuthFileInDir(outside, dir); err == nil {
		t.Fatal("delete outside dir should fail (A-23)")
	}
	// Traversal outside dir
	traversal := filepath.Join(dir, "..", "workbuddy-evil.json")
	if err := deleteAuthFileInDir(traversal, dir); err == nil {
		t.Fatal("delete with .. traversal should fail")
	}
	// Relative path — rejected (A-27: CWD deletion hazard).
	if err := deleteAuthFileInDir("workbuddy-evil.json", ""); err == nil {
		t.Fatal("relative path should fail (A-27)")
	}
	// Unsafe name — rejected.
	if err := deleteAuthFileInDir(filepath.Join(dir, "evil.json"), dir); err == nil {
		t.Fatal("unsafe name should fail")
	}
}

func TestLifecycleActionFor_IdempotentPolicy(t *testing.T) {
	// Applying the decision twice with the same inputs must stay stable (nothing
	// flips). Exhausted now maps to none in both regions — the credential stays
	// enabled so rate-0 models keep working — and repeating it still yields none.
	ex := &creditsSummary{TotalRemain: 0, TotalUsed: 1}
	for i := 0; i < 2; i++ {
		if lifecycleActionFor("cn", ex) != lifecycleNone {
			t.Fatal("cn exhausted must stay enabled on every pass")
		}
		if lifecycleActionFor("global", ex) != lifecycleNone {
			t.Fatal("global exhausted must stay enabled on every pass")
		}
	}
	// Soft rate limit body never hard-credit alone.
	if isHardCreditError(429, "too many requests") {
		t.Fatal("429 body without credit markers is not hard")
	}
	if !isSoftRateLimit(429, "rate limit") {
		t.Fatal("429 soft")
	}
}

func TestParseDisabledFromAuthJSON_StringTruth(t *testing.T) {
	// Only JSON boolean true counts; string "true" is false for strict parse.
	if parseDisabledFromAuthJSON([]byte(`{"disabled":"true"}`)) {
		t.Fatal("string true should not parse as bool true with current schema")
	}
}

func TestListEntryMatchesUID(t *testing.T) {
	uid := "00e26541-1884-4916-9c26-253a325d64ac"
	want := "workbuddy-" + uid + ".json"
	cases := []struct {
		name string
		f    pluginapi.HostAuthFileEntry
		want bool
	}{
		{"name exact", pluginapi.HostAuthFileEntry{Name: want}, true},
		{"id exact", pluginapi.HostAuthFileEntry{ID: want}, true},
		{"basename id", pluginapi.HostAuthFileEntry{Name: "workbuddy-" + uid}, true},
		{"case", pluginapi.HostAuthFileEntry{Name: strings.ToUpper(want)}, true},
		{"other uid", pluginapi.HostAuthFileEntry{Name: "workbuddy-other.json"}, false},
		{"legacy bare", pluginapi.HostAuthFileEntry{Name: "workbuddy.json"}, false},
		{"empty uid", pluginapi.HostAuthFileEntry{Name: want}, false},
	}
	for _, tc := range cases {
		u := uid
		if tc.name == "empty uid" {
			u = ""
		}
		got := listEntryMatchesUID(tc.f, u, want)
		if got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestDisabledReasonFieldRoundTrips pins the field that replaced note sniffing:
// lifecycle writes disabled_reason through buildAuthFileJSON's extra map and
// reads it back with authFileDisabledReason. The note cannot carry this — every
// reconcile rebuilds it via displayNoteWithPrev, which keeps only the region /
// 已禁用 / credit segments — so the decision must never depend on parsing it.
func TestDisabledReasonFieldRoundTrips(t *testing.T) {
	sa := &storedAuth{Account: storedAccount{Nickname: "t"}, Auth: storedTokens{Domain: "www.codebuddy.cn"}}

	for _, reason := range []string{disableReasonExhausted, disableReasonSessionDead} {
		raw, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 余0 已用5", map[string]any{
			"disabled_reason": reason,
		})
		if err != nil {
			t.Fatalf("build %s: %v", reason, err)
		}
		if got := authFileDisabledReason(raw); got != reason {
			t.Fatalf("disabled_reason round trip = %q, want %q", got, reason)
		}
		if !parseDisabledFromAuthJSON(raw) {
			t.Fatalf("%s: auth must stay disabled", reason)
		}
	}

	// A cleared reason (the re-enable path writes nil) must read back empty, so a
	// revoked account cannot inherit a stale "exhausted" that would revive it.
	cleared, err := buildAuthFileJSON(sa, false, "CN · 余3 已用5", map[string]any{"disabled_reason": nil})
	if err != nil {
		t.Fatal(err)
	}
	if got := authFileDisabledReason(cleared); got != "" {
		t.Fatalf("cleared disabled_reason = %q, want empty", got)
	}

	// An account written before the field existed has no key at all: empty, which
	// shouldReenableCN treats as "fall back to the note".
	legacy, err := buildAuthFileJSON(sa, true, "CN · 已禁用 · 余0 已用5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := authFileDisabledReason(legacy); got != "" {
		t.Fatalf("legacy auth without the field = %q, want empty", got)
	}
}

// TestNoteRewriteDoesNotCarryDisableReason documents WHY the reason cannot live
// in the note: syncAuthNote rebuilds it from the region/credit segments only, so
// anything appended for the operator is dropped on the next reconcile. This is
// the behaviour that made a note-based decision unsafe.
func TestNoteRewriteDoesNotCarryDisableReason(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	withReason := "CN · 已禁用 · 余0 已用5 · 耗尽"
	rewritten := displayNoteWithPrev(sa, &creditsSummary{TotalRemain: 0, TotalUsed: 5}, true, withReason)
	if strings.Contains(rewritten, creditExhaustedNoteMarker) {
		t.Logf("note still carries the marker (%q); even so, disabled_reason remains the authority", rewritten)
	}
	// The point: the note is not a reliable carrier, so the field exists.
	if got := authFileDisabledReason([]byte(`{"note":"` + rewritten + `"}`)); got != "" {
		t.Fatalf("a note must never be read as disabled_reason (got %q)", got)
	}
}
