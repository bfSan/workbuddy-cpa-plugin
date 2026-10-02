package main

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The panel refresh button must actually reach upstream. Before the force path
// existed, GET /models only re-unioned snapshots captured when each account was
// first seen, so pressing refresh showed whatever had been cached — the button
// was decorative.
func TestModelListRefreshPullsUpstreamInsteadOfServingCache(t *testing.T) {
	const (
		authID     = "auth-refresh"
		authIndex  = "acct-refresh"
		callbackID = "callback-refresh"
	)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)

	// Upstream publishes a different catalog on every call, so a stale cached
	// answer is distinguishable from a fresh one.
	var upstreamCalls atomic.Int64
	do := func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			n := upstreamCalls.Add(1)
			body := `{"code":0,"data":{"agents":[{"name":"cli","models":["serve-first"]}]}}`
			if n > 1 {
				body = `{"code":0,"data":{"agents":[{"name":"cli","models":["serve-second"]}]}}`
			}
			return &hostHTTPResponse{StatusCode: http.StatusOK, Headers: make(http.Header), Body: []byte(body)}, nil
		case "models.dev":
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"ETag": []string{`"refresh-etag"`}},
				Body: []byte(`{"vendor/serve-first":{"id":"serve-first","name":"First"},` +
					`"vendor/serve-second":{"id":"serve-second","name":"Second"}}`),
			}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: authID, AuthIndex: authIndex}}, nil
	}
	oldGet := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "workbuddy.json", JSON: mustJSON(sa)}, nil
	}
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
		hostAuthGetPhysicalFn = oldGet
	})
	defer setModelOverlayForTest(modelOverlay{})()

	requireCatalog := func(got map[string]any, wantID, context string) {
		t.Helper()
		items, ok := got["models"].([]map[string]any)
		if !ok {
			t.Fatalf("%s: models type = %T", context, got["models"])
		}
		seen := make(map[string]bool, len(items))
		for _, item := range items {
			id, _ := item["id"].(string)
			seen[id] = true
		}
		// applyModelOverlay may reorder or add, so assert membership rather than
		// an exact list.
		if !seen[wantID] {
			t.Fatalf("%s: catalog missing %s: %#v", context, wantID, seen)
		}
	}

	// CPA discovers each account through model.for_auth on registration; the
	// admin list only re-unions those snapshots. Pre-warm one so the list has a
	// catalog to show, exactly as it would in production.
	warmRequest := authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:       authID,
			AuthProvider: providerName,
			StorageJSON:  mustJSON(sa),
		},
		HostCallbackID: callbackID,
	}

	// First read discovers upstream and caches it.
	runtime.ensureForAuth(warmRequest)
	requireCatalog(handleModelListQuery(), "serve-first", "initial read")
	callsAfterFirst := upstreamCalls.Load()
	if callsAfterFirst == 0 {
		t.Fatal("initial read never reached upstream")
	}

	// A plain re-read must not hit upstream again: that is the cache doing its
	// job, and it is exactly why refresh needs its own switch.
	handleModelListQuery()
	if got := upstreamCalls.Load(); got != callsAfterFirst {
		t.Fatalf("plain re-read hit upstream %d more time(s), want cache hit", got-callsAfterFirst)
	}

	// The refresh button must bypass the cache and pick up the new catalog.
	refreshed := handleModelListQueryForce(true, callbackID)
	if err := refreshed["refresh_error"]; err != nil && err != "" {
		t.Fatalf("refresh_error = %v, want nil", err)
	}
	if got := upstreamCalls.Load(); got <= callsAfterFirst {
		t.Fatal("?refresh=1 did not reach upstream")
	}
	requireCatalog(refreshed, "serve-second", "forced refresh")
}

// A transient upstream failure during refresh must not strip a working catalog:
// dropping the last good snapshot would empty the panel and live routing until
// the next successful discovery.
func TestModelListRefreshFailureKeepsLastGoodCatalog(t *testing.T) {
	const (
		authID     = "auth-refresh-fail"
		authIndex  = "acct-refresh-fail"
		callbackID = "callback-refresh-fail"
	)
	sa := syntheticStoredAuth(t, workBuddyRealmCN)

	var fail atomic.Bool
	do := func(req *http.Request, gotCallbackID string) (*hostHTTPResponse, error) {
		switch req.URL.Host {
		case "copilot.tencent.com":
			if fail.Load() {
				return &hostHTTPResponse{StatusCode: http.StatusInternalServerError, Headers: make(http.Header), Body: []byte(`{"code":1}`)}, nil
			}
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    make(http.Header),
				Body:       []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-stable"]}]}}`),
			}, nil
		case "models.dev":
			return &hostHTTPResponse{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"ETag": []string{`"fail-etag"`}},
				Body:       []byte(`{"vendor/serve-stable":{"id":"serve-stable","name":"Stable"}}`),
			}, nil
		default:
			t.Fatalf("unexpected model request %s", req.URL)
			return nil, nil
		}
	}

	runtime := newModelRuntime(newModelStore(t.TempDir()), do)
	oldRuntime := activeModelRuntime.Swap(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: authID, AuthIndex: authIndex}}, nil
	}
	oldGet := hostAuthGetPhysicalFn
	hostAuthGetPhysicalFn = func(string) (*hostAuthPhysical, error) {
		return &hostAuthPhysical{AuthIndex: authIndex, Name: "workbuddy.json", JSON: mustJSON(sa)}, nil
	}
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
		hostAuthGetPhysicalFn = oldGet
	})
	defer setModelOverlayForTest(modelOverlay{})()

	// Establish a good catalog the way CPA would: discovery via model.for_auth,
	// then the admin list re-unions the resulting snapshot.
	runtime.ensureForAuth(authModelRequestWire{
		AuthModelRequest: pluginapi.AuthModelRequest{
			AuthID:       authID,
			AuthProvider: providerName,
			StorageJSON:  mustJSON(sa),
		},
		HostCallbackID: callbackID,
	})
	first := handleModelListQuery()
	hasStable := func(got map[string]any) bool {
		items, _ := got["models"].([]map[string]any)
		for _, item := range items {
			if item["id"] == "serve-stable" {
				return true
			}
		}
		return false
	}
	if !hasStable(first) {
		t.Fatalf("initial catalog missing serve-stable: %#v", first["models"])
	}

	// Now break upstream and refresh anyway.
	fail.Store(true)
	failed := handleModelListQueryForce(true, callbackID)
	if _, ok := failed["refresh_error"]; !ok {
		t.Fatal("refresh_error missing; the panel could not tell the refresh failed")
	}
	if !hasStable(failed) {
		t.Fatalf("failed refresh dropped the last good catalog: %#v", failed["models"])
	}
}
