package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The panel has always shown per-(account, model) throttling, but model.for_auth
// ignored it, so CPA kept treating a throttled pair as healthy and a
// session-affinity binding kept routing into it until the TTL expired. These
// tests pin the filter that closes that gap.

// coolingCatalogRuntime serves a fixed catalog so the test can focus on the
// cooldown filter rather than on discovery.
func coolingCatalogRuntime(t *testing.T, callbackID string, ids ...string) *modelRuntime {
	t.Helper()
	agents := make([]string, 0, len(ids))
	agents = append(agents, ids...)
	raw, err := json.Marshal(map[string]any{
		"code": 0,
		"data": map[string]any{"agents": []map[string]any{{"name": "cli", "models": agents}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	modelsDev := map[string]any{}
	for _, id := range ids {
		modelsDev["vendor/"+id] = map[string]any{"id": id, "name": "Name " + id}
	}
	modelsDevRaw, err := json.Marshal(modelsDev)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		if gotCallbackID != callbackID {
			t.Fatalf("callback ID = %q, want %q", gotCallbackID, callbackID)
		}
		switch req.URL.Host {
		case "copilot.tencent.com":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: raw}, nil
		case "models.dev":
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"ETag": []string{`"cooling-etag"`}}, Body: modelsDevRaw}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	})
	return runtime
}

func servingModelIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	resp := decodeModelResponse(t, raw)
	ids := make([]string, 0, len(resp.Models))
	for _, m := range resp.Models {
		ids = append(ids, m.ID)
	}
	return ids
}

func forAuthWithAuthID(t *testing.T, runtime *modelRuntime, authID, callbackID string) []string {
	t.Helper()
	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: authID, AuthIndex: "acct-" + authID}}, nil
	}
	defer func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	}()
	raw, err := handleModelForAuth(mustJSON(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{AuthID: authID, StorageJSON: mustJSON(sa)},
		HostCallbackID:   callbackID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	return servingModelIDs(t, raw)
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestForAuthWithholdsOnlyTheCoolingModel(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-cooling"
		callbackID = "callback-cooling"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta", "gamma")
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	ids := forAuthWithAuthID(t, runtime, authID, callbackID)
	if containsID(ids, "beta") {
		t.Fatalf("cooling model still served to CPA: %v", ids)
	}
	// The rest of the account's catalog must survive: cooldown is per model by
	// design, and blacking out the whole account would turn one degraded model
	// into an auth-wide outage.
	if !containsID(ids, "alpha") || !containsID(ids, "gamma") {
		t.Fatalf("healthy models were dropped along with the cooling one: %v", ids)
	}
	if len(ids) != 2 {
		t.Fatalf("served %v, want alpha and gamma only", ids)
	}
}

func TestForAuthIgnoresAnotherAccountsCooldown(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-mine"
		otherAuth  = "auth-other"
		callbackID = "callback-scope"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta")
	// Another account is cooling beta. That says nothing about this account,
	// and applying it here would hide a working model.
	markModelCooldown(otherAuth, "beta", cooldownReasonRateLimit)
	markModelCooldown(otherAuth, "alpha", cooldownReasonRateLimit)

	ids := forAuthWithAuthID(t, runtime, authID, callbackID)
	if !containsID(ids, "alpha") || !containsID(ids, "beta") {
		t.Fatalf("another account's cooldown leaked into this auth: %v", ids)
	}
}

func TestForAuthRestoresModelAfterCooldownExpires(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-expiry"
		callbackID = "callback-expiry"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta")

	base := time.Now()
	cooldownNowFn = func() time.Time { return base }
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	if ids := forAuthWithAuthID(t, runtime, authID, callbackID); containsID(ids, "beta") {
		t.Fatalf("cooling model served before expiry: %v", ids)
	}
	// Past the window the pair is healthy again and must come back, otherwise a
	// transient throttle would permanently shrink the catalog.
	cooldownNowFn = func() time.Time { return base.Add(modelCooldownRateLimit + time.Minute) }
	ids := forAuthWithAuthID(t, runtime, authID, callbackID)
	if !containsID(ids, "beta") {
		t.Fatalf("model did not return after the cooldown expired: %v", ids)
	}
}

// A cooldown recorded against the upstream name must still hide the catalog ID
// when an alias sits between them; otherwise the filter would silently miss.
func TestForAuthMatchesCoolingRecordedAgainstUpstreamAlias(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-alias"
		callbackID = "callback-alias"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alias-model", "other-model")
	markModelCooldown(authID, "upstream-model", cooldownReasonRateLimit)

	sa := syntheticStoredAuth(t, workBuddyRealmCN)
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: authID, AuthIndex: "acct-alias"}}, nil
	}
	defer func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	}()

	raw, err := handleModelForAuth(mustJSON(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:      authID,
			StorageJSON: mustJSON(sa),
			// The auth maps the catalog ID to the upstream model the cooldown
			// was recorded against.
			Attributes: map[string]string{"model_alias": `[{"alias":"alias-model","name":"upstream-model"}]`},
		},
		HostCallbackID: callbackID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	ids := servingModelIDs(t, raw)
	if containsID(ids, "alias-model") {
		t.Fatalf("aliased cooling model was still served: %v", ids)
	}
	if !containsID(ids, "other-model") {
		t.Fatalf("unrelated model was dropped: %v", ids)
	}
}

// A cooldown that matches nothing must not thin the catalog: a missed filter
// keeps today's behaviour, a false positive makes a working model vanish.
func TestForAuthKeepsEveryModelWhenNothingMatches(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-nomatch"
		callbackID = "callback-nomatch"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta")
	markModelCooldown(authID, "some-other-model", cooldownReasonRateLimit)

	ids := forAuthWithAuthID(t, runtime, authID, callbackID)
	if len(ids) != 2 {
		t.Fatalf("unrelated cooldown removed models: %v", ids)
	}
}

// The panel's admin catalog must keep exposing a cooling model, because the
// operator has to see it in order to clear it.
func TestCoolingDoesNotHideModelFromAdminCatalog(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-admin"
		callbackID = "callback-admin"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta")
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	ids := forAuthWithAuthID(t, runtime, authID, callbackID)
	if containsID(ids, "beta") {
		t.Fatalf("precondition failed, beta should be withheld from CPA: %v", ids)
	}

	// The runtime snapshot is what the admin catalog is built from.
	if snapshot := runtime.snapshotForAuthID(authID); len(snapshot.Models) != 2 {
		t.Fatalf("cooldown filter leaked into the runtime snapshot: %#v", snapshot.Models)
	}
}

// The account's own catalog must not be mutated by the filter: it is served to
// CPA as a copy, but a later request has to see the model again once it recovers.
func TestForAuthDoesNotMutateSnapshotAcrossCalls(t *testing.T) {
	resetCooldowns(t)
	const (
		authID     = "auth-stable"
		callbackID = "callback-stable"
	)
	runtime := coolingCatalogRuntime(t, callbackID, "alpha", "beta")
	markModelCooldown(authID, "beta", cooldownReasonRateLimit)

	first := forAuthWithAuthID(t, runtime, authID, callbackID)
	second := forAuthWithAuthID(t, runtime, authID, callbackID)
	if len(first) != len(second) {
		t.Fatalf("repeated calls disagree: %v vs %v", first, second)
	}
	if snapshot := runtime.snapshotForAuthID(authID); len(snapshot.Models) != 2 {
		t.Fatalf("snapshot shrank after repeated filters: %#v", snapshot.Models)
	}
}

// A cooldown with no auth ID cannot be scoped, and guessing would risk hiding a
// healthy model, so the catalog must pass through untouched.
func TestForAuthWithoutAuthIDKeepsCatalog(t *testing.T) {
	resetCooldowns(t)
	models := []pluginapi.ModelInfo{{ID: "alpha"}, {ID: "beta"}}
	markModelCooldown("some-auth", "alpha", cooldownReasonRateLimit)

	if got := filterCoolingModels(authModelRequestWire{}, models); len(got) != 2 {
		t.Fatalf("unscoped filter removed models: %#v", got)
	}
}

// coolingModelSetFor must report only the requested account, and only live
// entries.
func TestCoolingModelSetForScopesAndSweeps(t *testing.T) {
	resetCooldowns(t)
	base := time.Now()
	cooldownNowFn = func() time.Time { return base }
	markModelCooldown("auth-a", "alpha", cooldownReasonRateLimit)
	markModelCooldown("auth-a", "beta", cooldownReasonRateLimit)
	markModelCooldown("auth-b", "gamma", cooldownReasonRateLimit)

	set := coolingModelSetFor("auth-a")
	if len(set) != 2 {
		t.Fatalf("auth-a set = %#v, want alpha and beta", set)
	}
	if _, ok := set["gamma"]; ok {
		t.Fatalf("auth-b leaked into auth-a's set: %#v", set)
	}

	// An expired entry must be swept rather than reported.
	cooldownNowFn = func() time.Time { return base.Add(modelCooldownRateLimit + time.Minute) }
	if got := coolingModelSetFor("auth-a"); len(got) != 0 {
		t.Fatalf("expired entries still reported: %#v", got)
	}
}

// CPA decides the cooldown it records from the status on the error, so the type
// has to expose one. Without it a 429 that this plugin throttles for five
// minutes reached CPA as an unclassified error.
func TestUpstreamStatusErrorExposesStatusCode(t *testing.T) {
	err := &upstreamStatusError{status: http.StatusTooManyRequests, message: "upstream 429"}
	var sc interface{ StatusCode() int }
	if !errors.As(err, &sc) {
		t.Fatal("upstreamStatusError does not expose StatusCode")
	}
	if got := sc.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode() = %d, want 429", got)
	}
	// A nil receiver must not panic; the pump can hand one over through
	// errors.As after the value was cleared.
	var nilErr *upstreamStatusError
	if got := nilErr.StatusCode(); got != 0 {
		t.Fatalf("nil receiver StatusCode() = %d, want 0", got)
	}
}

// The top-level dispatcher must not flatten a status-bearing error into a
// status-less envelope: CPA sizes its credential cooldown from that status, so
// dropping it made a 429 that this plugin throttles for five minutes arrive
// unclassified and only earn the short transient default.
func TestErrorEnvelopeForPreservesStatusCode(t *testing.T) {
	raw := errorEnvelopeFor(&upstreamStatusError{status: http.StatusTooManyRequests, message: "upstream 429: quota"})
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		t.Fatalf("envelope has no error: %s", raw)
	}
	if env.Error.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("http_status = %d, want 429 (envelope %s)", env.Error.HTTPStatus, raw)
	}
}

// An error with no status must still render a usable envelope rather than
// claiming a status it does not have.
func TestErrorEnvelopeForOmitsUnknownStatus(t *testing.T) {
	raw := errorEnvelopeFor(errors.New("plain failure"))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("envelope = %s", raw)
	}
	if env.Error.HTTPStatus != 0 {
		t.Fatalf("http_status = %d, want 0", env.Error.HTTPStatus)
	}
}
