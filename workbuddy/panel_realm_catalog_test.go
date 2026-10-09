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
