package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestBuildPanelRealmCatalogKeepsCNAndGlobalFactsSeparate(t *testing.T) {
	oldRuntime := activeModelRuntime.Load()
	oldList := panelHostAuthList
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	})

	cnBase, globalBase := int64(128000), int64(1000000)
	cn := modelReadinessSnapshot{
		Realm:           workBuddyRealmCN,
		State:           modelReady,
		ModelSource:     modelSourceFresh,
		ModelsFetchedAt: time.Unix(100, 0),
		ModelFacts: []modelFacts{{
			ID: "shared", Name: "CN Shared", Credits: "x0.21",
			ContextLength: &cnBase, SupportedContextLengths: []int64{128000},
		}},
	}
	global := modelReadinessSnapshot{
		Realm:           workBuddyRealmGlobal,
		State:           modelReady,
		ModelSource:     modelSourceFresh,
		ModelsFetchedAt: time.Unix(200, 0),
		ModelFacts: []modelFacts{{
			ID: "shared", Name: "Global Shared", Credits: "x0.29",
			ContextLength: &globalBase, SupportedContextLengths: []int64{1000000},
		}, {ID: "global-only", Credits: "x0.00"}},
	}

	r := newModelRuntime(newModelStore(t.TempDir()), nil)
	cnSlot := &modelAuthSlot{}
	cnSlot.current.Store(&cn)
	globalSlot := &modelAuthSlot{}
	globalSlot.current.Store(&global)
	r.authSlots.Store("cn-auth", cnSlot)
	r.authSlots.Store("global-auth", globalSlot)
	activeModelRuntime.Store(r)
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "cn-auth"}, {ID: "global-auth"}}, nil
	}

	rows, status := buildPanelRealmCatalog()
	if len(rows) != 2 {
		t.Fatalf("rows=%d, want two unified IDs", len(rows))
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	shared := byID["shared"]
	if shared == nil {
		t.Fatal("shared row missing")
	}
	if got := shared["cn"].(map[string]any)["credits"].(map[string]any)["value"]; got != "x0.21" {
		t.Fatalf("CN credits=%v", got)
	}
	if got := shared["global"].(map[string]any)["credits"].(map[string]any)["value"]; got != "x0.29" {
		t.Fatalf("Global credits=%v", got)
	}
	if got := shared["cn"].(map[string]any)["context_length"].(*int64); *got != cnBase {
		t.Fatalf("CN context=%v", *got)
	}
	if got := shared["global"].(map[string]any)["context_length"].(*int64); *got != globalBase {
		t.Fatalf("Global context=%v", *got)
	}
	if got := byID["global-only"]["cn"].(map[string]any)["status"]; got != "absent" {
		t.Fatalf("CN status=%v", got)
	}
	if got := status["cn"].(map[string]any)["loaded"]; got != true {
		t.Fatalf("CN loaded=%v", got)
	}
	if got := status["global"].(map[string]any)["loaded"]; got != true {
		t.Fatalf("Global loaded=%v", got)
	}

	// Returned facts must not alias the readiness snapshot.
	*shared["cn"].(map[string]any)["context_length"].(*int64)++
	if cn.ModelFacts[0].ContextLength == nil || *cn.ModelFacts[0].ContextLength != cnBase {
		t.Fatal("catalog row aliases source facts")
	}
	if !reflect.DeepEqual(shared["cn"].(map[string]any)["thinking"], map[string]any{"status": "unknown", "levels": []string{}}) {
		t.Fatal("thinking was guessed")
	}
}
