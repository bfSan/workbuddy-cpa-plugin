package main

import (
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

	// Returned slices must not alias the readiness snapshot: a caller mutating a
	// response must not corrupt the catalog another request reads.
	supported := shared["cn"].(map[string]any)["supported_context_lengths"].([]int64)
	if len(supported) == 0 {
		t.Fatal("supported context lengths were dropped")
	}
	supported[0]++
	if cn.ModelFacts[0].SupportedContextLengths[0] != 128000 {
		t.Fatal("catalog row aliases source facts")
	}
	if levels := shared["cn"].(map[string]any)["thinking"].(map[string]any)["levels"].([]string); len(levels) > 0 {
		levels[0] = "mutated"
		if len(cn.ModelFacts[0].SupportedEfforts) > 0 && cn.ModelFacts[0].SupportedEfforts[0] == "mutated" {
			t.Fatal("thinking levels alias source facts")
		}
	}
}

// An unreported reasoning capability must stay unknown. Reporting unsupported
// would tell the operator a model cannot think when upstream simply did not say,
// and inventing levels would send them to a value the upstream rejects.
func TestPanelRealmThinkingSeparatesUnreportedFromUnsupported(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name  string
		facts modelFacts
		want  string
	}{
		{"levels reported", modelFacts{SupportedEfforts: []string{"low", "high"}}, "supported"},
		{"explicitly unsupported", modelFacts{SupportsReasoning: &no}, "unsupported"},
		{"nothing reported", modelFacts{}, "unknown"},
		{"reasoning capable but no levels", modelFacts{SupportsReasoning: &yes}, "unknown"},
		{"disable-only", modelFacts{CanDisableThinking: &yes}, "off_only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := realmFactThinkingValue(tc.facts)
			if got["status"] != tc.want {
				t.Fatalf("status = %v, want %v", got["status"], tc.want)
			}
			if tc.want != "supported" && len(got["levels"].([]string)) != 0 {
				t.Fatalf("levels invented for %v: %v", tc.want, got["levels"])
			}
		})
	}
	// An omitted canDisableThinking must stay null rather than defaulting to
	// false, which would claim the operator cannot turn thinking off. The value
	// is a typed *bool, so read it through the pointer instead of comparing
	// against an untyped nil interface, which a nil pointer never equals.
	got := realmFactThinkingValue(modelFacts{})["can_disable"]
	if ptr, ok := got.(*bool); !ok || ptr != nil {
		t.Fatalf("can_disable = %#v, want a nil *bool when upstream omitted it", got)
	}
}

// installRealmCatalogForTest wires N auth slots into a fresh runtime and points
// the panel's auth list at them, so a realm can be given several accounts.
func installRealmCatalogForTest(t *testing.T, snapshots map[string]modelReadinessSnapshot) {
	t.Helper()
	oldRuntime := activeModelRuntime.Load()
	oldList := panelHostAuthList
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	})

	r := newModelRuntime(newModelStore(t.TempDir()), nil)
	ids := make([]string, 0, len(snapshots))
	for id, snap := range snapshots {
		snapshot := snap
		slot := &modelAuthSlot{}
		slot.current.Store(&snapshot)
		r.authSlots.Store(id, slot)
		ids = append(ids, id)
	}
	activeModelRuntime.Store(r)
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		entries := make([]pluginapi.HostAuthFileEntry, 0, len(ids))
		for _, id := range ids {
			entries = append(entries, pluginapi.HostAuthFileEntry{ID: id})
		}
		return entries, nil
	}
}

// Upstream trims the catalog per account: two CN accounts can each advertise
// models the other never receives. Selecting one snapshot made the other
// account's exclusives vanish, and the panel rendered "未加载" for a model the
// realm actually serves. Observed live as models=71 vs realm_models=69, the two
// missing IDs being exactly one account's exclusives.
func TestPanelRealmUnionKeepsEveryAccountsExclusiveModels(t *testing.T) {
	base := time.Unix(500, 0)
	installRealmCatalogForTest(t, map[string]modelReadinessSnapshot{
		"cn-account-a": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: base,
			ModelFacts: []modelFacts{{ID: "shared"}, {ID: "minimax-m3"}, {ID: "kimi-k3-1"}},
		},
		"cn-account-b": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: base.Add(time.Second),
			ModelFacts: []modelFacts{{ID: "shared"}, {ID: "glm-5.3-flashx"}, {ID: "step-5-preview"}},
		},
	})

	rows, status := buildPanelRealmCatalog()
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	for _, want := range []string{"shared", "minimax-m3", "kimi-k3-1", "glm-5.3-flashx", "step-5-preview"} {
		row, ok := byID[want]
		if !ok {
			t.Fatalf("union dropped %q; account exclusives must survive", want)
		}
		cell := row["cn"].(map[string]any)
		if cell["status"] != "present" {
			t.Fatalf("%q cn status = %v, want present (the realm does serve it)", want, cell["status"])
		}
	}
	if loaded, _ := status["cn"].(map[string]any)["loaded"].(bool); !loaded {
		t.Fatal("cn should be loaded")
	}
	// Global has no account at all, so its column is genuinely not loaded and
	// must not be reported as a plain absence.
	if got := byID["shared"]["global"].(map[string]any)["status"]; got != "not_loaded" {
		t.Fatalf("global status = %v, want not_loaded", got)
	}
	if loaded, _ := status["global"].(map[string]any)["loaded"].(bool); loaded {
		t.Fatal("global has no snapshot and must not report loaded")
	}
}

// A duplicate ID across accounts must resolve to the same winner on every call,
// otherwise credits and context visibly jump between panel refreshes.
func TestPanelRealmUnionDuplicateIDResolvesDeterministically(t *testing.T) {
	older, newer := time.Unix(100, 0), time.Unix(200, 0)
	oldCredits, newCredits := "x0.10", "x0.90"
	installRealmCatalogForTest(t, map[string]modelReadinessSnapshot{
		"cn-older": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: older,
			ModelFacts: []modelFacts{{ID: "dup", Credits: oldCredits}},
		},
		"cn-newer": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: newer,
			ModelFacts: []modelFacts{{ID: "dup", Credits: newCredits}},
		},
	})

	var first string
	for attempt := 0; attempt < 20; attempt++ {
		rows, _ := buildPanelRealmCatalog()
		var got string
		for _, row := range rows {
			if row["id"] == "dup" {
				credits := row["cn"].(map[string]any)["credits"].(map[string]any)
				got, _ = credits["value"].(string)
			}
		}
		if got == "" {
			t.Fatal("dup row missing")
		}
		if attempt == 0 {
			first = got
			if got != newCredits {
				t.Fatalf("dup credits = %q, want the newest snapshot's %q", got, newCredits)
			}
			continue
		}
		if got != first {
			t.Fatalf("duplicate resolution not deterministic: %q then %q", first, got)
		}
	}
}

// A realm with facts still reports a missing model as absent ("—"), never as
// not_loaded ("未加载"): the two make different claims about the realm.
func TestPanelRealmDistinguishesAbsentFromNotLoaded(t *testing.T) {
	installRealmCatalogForTest(t, map[string]modelReadinessSnapshot{
		"cn-account": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: time.Unix(300, 0),
			ModelFacts: []modelFacts{{ID: "cn-only"}},
		},
		"global-account": {
			Realm: workBuddyRealmGlobal, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: time.Unix(300, 0),
			ModelFacts: []modelFacts{{ID: "global-only"}},
		},
	})

	rows, status := buildPanelRealmCatalog()
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[row["id"].(string)] = row
	}
	if got := byID["cn-only"]["global"].(map[string]any)["status"]; got != "absent" {
		t.Fatalf("cn-only global = %v, want absent", got)
	}
	if got := byID["global-only"]["cn"].(map[string]any)["status"]; got != "absent" {
		t.Fatalf("global-only cn = %v, want absent", got)
	}
	for realm, want := range map[string]string{"cn": "cn-only", "global": "global-only"} {
		if loaded, _ := status[realm].(map[string]any)["loaded"].(bool); !loaded {
			t.Fatalf("%s should be loaded", realm)
		}
		if got := byID[want][realm].(map[string]any)["status"]; got != "present" {
			t.Fatalf("%s %s = %v, want present", realm, want, got)
		}
	}
}

// status and rows come from one selection, so a loaded realm must never render a
// not_loaded cell: that combination is what made the panel look broken.
func TestPanelRealmStatusAndRowsAgree(t *testing.T) {
	installRealmCatalogForTest(t, map[string]modelReadinessSnapshot{
		"cn-account": {
			Realm: workBuddyRealmCN, State: modelReady,
			ModelSource: modelSourceFresh, ModelsFetchedAt: time.Unix(400, 0),
			ModelFacts: []modelFacts{{ID: "a"}, {ID: "b"}},
		},
	})
	rows, status := buildPanelRealmCatalog()
	for realm, entry := range status {
		loaded, _ := entry.(map[string]any)["loaded"].(bool)
		for _, row := range rows {
			cell, ok := row[realm].(map[string]any)
			if !ok {
				continue
			}
			if loaded && cell["status"] == "not_loaded" {
				t.Fatalf("realm %s reports loaded but row %v is not_loaded", realm, row["id"])
			}
			if !loaded && cell["status"] == "present" {
				t.Fatalf("realm %s reports not loaded but row %v is present", realm, row["id"])
			}
		}
	}
}

// A failed snapshot must not make its realm look unloaded, and its failure must
// still be visible in status.
func TestPanelRealmFailedAccountKeepsRealmUnloadedButNamed(t *testing.T) {
	installRealmCatalogForTest(t, map[string]modelReadinessSnapshot{
		"cn-broken": {
			Realm: workBuddyRealmCN, State: modelFailed,
			ModelSource: modelSourceNone, ErrorCode: modelErrorWorkBuddyTransport,
		},
	})
	rows, status := buildPanelRealmCatalog()
	cn := status["cn"].(map[string]any)
	if loaded, _ := cn["loaded"].(bool); loaded {
		t.Fatal("a failed snapshot must not report loaded")
	}
	if cn["status"] != string(modelFailed) {
		t.Fatalf("cn status = %v, want the failure state", cn["status"])
	}
	if len(rows) != 0 {
		t.Fatalf("a failed realm should contribute no model rows, got %d", len(rows))
	}
}
