package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func withModelContextOverrides(t *testing.T, overrides map[string]int64) {
	t.Helper()
	old := featureRuntime.Load()
	t.Cleanup(func() { featureRuntime.Store(old) })
	cfg := currentFeatureRuntime()
	next := *cfg
	next.modelContext = overrides
	featureRuntime.Store(&next)
}

func resetModelContextCatalog(t *testing.T) {
	t.Helper()
	modelContextCatalog.Lock()
	old := modelContextCatalog.byModel
	modelContextCatalog.byModel = make(map[string]contextMetadata)
	modelContextCatalog.Unlock()
	t.Cleanup(func() {
		modelContextCatalog.Lock()
		modelContextCatalog.byModel = old
		modelContextCatalog.Unlock()
	})
}

// The provider reports the full capability list; the plugin must keep every
// tier so the panel can offer it, while CPA's scalar ContextLength keeps the
// model's real maximum instead of the provider's smaller default tier.
func TestParseV3ConfigKeepsContextWindowMetadata(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}],
		"models":[{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,
		"maxAllowedSize":1000000,"maxOutputTokens":128000,
		"contextWindow":{"defaultLength":300000,"supportedLengths":[300000,600000,1000000]}}]}}`)

	facts, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("facts = %d, want 1", len(facts))
	}
	got := facts[0]
	if got.DefaultContextLength == nil || *got.DefaultContextLength != 300000 {
		t.Fatalf("default context = %v, want 300000", got.DefaultContextLength)
	}
	if got.MaxAllowedSize == nil || *got.MaxAllowedSize != 1000000 {
		t.Fatalf("maxAllowedSize = %v, want 1000000", got.MaxAllowedSize)
	}
	if got.MaxInputTokens == nil || *got.MaxInputTokens != 1000000 {
		t.Fatalf("maxInputTokens = %v, want 1000000", got.MaxInputTokens)
	}
	want := []int64{300000, 600000, 1000000}
	if len(got.SupportedContextLengths) != len(want) {
		t.Fatalf("supported = %v, want %v", got.SupportedContextLengths, want)
	}
	for i := range want {
		if got.SupportedContextLengths[i] != want[i] {
			t.Fatalf("supported = %v, want %v", got.SupportedContextLengths, want)
		}
	}
}

// Older accounts/fixtures answer with a bare numeric contextWindow. That must
// still parse instead of failing the whole catalog with a JSON type error.
func TestParseV3ConfigAcceptsLegacyNumericContextWindow(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"agents":[{"name":"cli","models":["serve-alpha"]}],
		"models":[{"id":"serve-alpha","name":"Alpha","contextWindow":4096,"maxOutputTokens":512}]}}`)

	facts, err := parseWorkBuddyV3Config(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if facts[0].DefaultContextLength == nil || *facts[0].DefaultContextLength != 4096 {
		t.Fatalf("default context = %v, want 4096", facts[0].DefaultContextLength)
	}
	if facts[0].ContextLength == nil || *facts[0].ContextLength != 4096 {
		t.Fatalf("context length = %v, want 4096", facts[0].ContextLength)
	}
}

func TestEffectiveModelContextPrefersMaxWithoutOverride(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, nil)
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		MaxAllowedSize:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	length, source, options := effectiveModelContext("deepseek-v4.1-flash", 1000000)
	if length != 1000000 || source != "upstream" {
		t.Fatalf("effective = %d/%s, want 1000000/upstream", length, source)
	}
	if len(options) != 3 {
		t.Fatalf("options = %v, want 3 tiers", options)
	}
}

func TestEffectiveModelContextUsesOverride(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, map[string]int64{"deepseek-v4.1-flash": 600000})
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	length, source, _ := effectiveModelContext("deepseek-v4.1-flash", 1000000)
	if length != 600000 || source != "override" {
		t.Fatalf("effective = %d/%s, want 600000/override", length, source)
	}
}

// The selected tier is metadata only. It must never leak into the upstream chat
// body: /v2/chat/completions ignores a body context_window field, so sending one
// would be an unverified guess. This locks the request body as untouched even
// when an override IS set.
func TestPrepareUpstreamBodyNeverInjectsContextWindow(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, map[string]int64{"deepseek-v4.1-flash": 1000000})
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	out := prepareUpstreamBody([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), nil, nil, "deepseek-v4.1-flash")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, exists := obj["context_window"]; exists {
		t.Fatalf("context_window must not be sent upstream: %s", out)
	}
	// The unrelated rewrites must still work.
	if obj["stream"] != true {
		t.Fatalf("stream = %v, want true", obj["stream"])
	}
	if obj["model"] != "deepseek-v4.1-flash" {
		t.Fatalf("model = %v, want rewritten id", obj["model"])
	}
}

func TestModelContextWriteRejectsUnsupportedTier(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, nil)
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	res := handleModelContextWrite(managementRequestWithBody(`{"model":"deepseek-v4.1-flash","context_length":512000}`))
	if res["success"] != false {
		t.Fatalf("unsupported tier accepted: %v", res)
	}
	if _, ok := res["context_options"]; !ok {
		t.Fatalf("error should report the supported tiers: %v", res)
	}
}

func TestModelContextWriteStoresAndClearsOverride(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, nil)
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	res := handleModelContextWrite(managementRequestWithBody(`{"model":"deepseek-v4.1-flash","context_length":600000}`))
	if res["success"] != true {
		t.Fatalf("valid tier rejected: %v", res)
	}
	stored, ok := res["model_context"].(map[string]int64)
	if !ok || stored["deepseek-v4.1-flash"] != 600000 {
		t.Fatalf("override not stored: %v", res["model_context"])
	}

	cleared := handleModelContextWrite(managementRequestWithBody(`{"model":"deepseek-v4.1-flash","context_length":null}`))
	after, _ := cleared["model_context"].(map[string]int64)
	if _, exists := after["deepseek-v4.1-flash"]; exists {
		t.Fatalf("override not cleared: %v", after)
	}
}

func TestModelListQueryReportsContextFields(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, map[string]int64{"deepseek-v4.1-flash": 600000})
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})
	defer setModelOverlayForTest(modelOverlay{})()
	oldRuntime := activeModelRuntime.Load()
	activeModelRuntime.Store(newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		return nil, nil
	}))
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "auth-context", AuthIndex: "idx-context"}}, nil
	}
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	})

	snapshot := modelReadinessSnapshot{
		State:       modelReady,
		ModelSource: modelSourceConfig,
		Models:      []pluginapi.ModelInfo{{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash"}},
	}
	runtime := currentModelRuntime()
	slot := runtime.authSlot("auth-context")
	storeModelReadinessSnapshot(slot, snapshot)

	resp := handleModelListQuery()
	items, ok := resp["models"].([]map[string]any)
	if !ok || len(items) == 0 {
		t.Fatalf("models payload missing: %#v", resp["models"])
	}
	item := items[0]
	if item["context_length"] != int64(600000) || item["context_source"] != "override" {
		t.Fatalf("context fields = %v/%v, want 600000/override", item["context_length"], item["context_source"])
	}
	options, ok := item["context_options"].([]int64)
	if !ok || len(options) != 3 {
		t.Fatalf("context_options = %#v, want 3 tiers", item["context_options"])
	}
}

func TestNormalizedModelContextConfigValidates(t *testing.T) {
	if _, err := parseFeatureRuntime([]byte("model_context:\n  m1: 300000\n")); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if _, err := parseFeatureRuntime([]byte("model_context:\n  m1: 0\n")); err == nil {
		t.Fatal("zero value accepted")
	}
	if _, err := parseFeatureRuntime([]byte("model_context:\n  m1: -5\n")); err == nil {
		t.Fatal("negative value accepted")
	}
	if _, err := parseFeatureRuntime([]byte("model_context:\n  m1: abc\n")); err == nil {
		t.Fatal("non-numeric value accepted")
	}
	if _, err := parseFeatureRuntime([]byte("model_context: []\n")); err == nil {
		t.Fatal("sequence accepted")
	}
}

func ptrInt64(v int64) *int64 { return &v }

func TestRemovedContextOverridesDetectsCleared(t *testing.T) {
	previous := &featureRuntimeConfig{modelContext: map[string]int64{"a": 1, "b": 2}}
	next := &featureRuntimeConfig{modelContext: map[string]int64{"a": 1}}
	removed := changedContextOverrides(previous, next)
	if _, ok := removed["b"]; !ok || len(removed) != 1 {
		t.Fatalf("changed = %v, want only b", removed)
	}
	if got := changedContextOverrides(previous, &featureRuntimeConfig{modelContext: map[string]int64{"a": 1, "b": 2}}); len(got) != 0 {
		t.Fatalf("unchanged config reported changes: %v", got)
	}
	// A changed value must also be reported, not just a removal.
	changed := changedContextOverrides(previous, &featureRuntimeConfig{modelContext: map[string]int64{"a": 5, "b": 2}})
	if _, ok := changed["a"]; !ok || len(changed) != 1 {
		t.Fatalf("value change not detected: %v", changed)
	}
	// A newly added override counts too.
	added := changedContextOverrides(previous, &featureRuntimeConfig{modelContext: map[string]int64{"a": 1, "b": 2, "c": 3}})
	if _, ok := added["c"]; !ok {
		t.Fatalf("added override not detected: %v", added)
	}
}

// The panel persists through the host config PATCH, which lands in
// commitFeatureRuntime rather than the plugin's own write endpoint. Clearing an
// override there must also roll snapshots back.
func TestCommitFeatureRuntimeClearsSnapshotOnOverrideRemoval(t *testing.T) {
	resetModelContextCatalog(t)
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})
	recordBaseContextLength("deepseek-v4.1-flash", 1000000)

	oldRuntime := activeModelRuntime.Load()
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		return nil, nil
	})
	activeModelRuntime.Store(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "auth-commit", AuthIndex: "idx-commit"}}, nil
	}
	oldFeatures := featureRuntime.Load()
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
		featureRuntime.Store(oldFeatures)
	})

	slot := runtime.authSlot("auth-commit")
	storeModelReadinessSnapshot(slot, modelReadinessSnapshot{
		State:       modelReady,
		ModelSource: modelSourceFresh,
		Models:      []pluginapi.ModelInfo{{ID: "deepseek-v4.1-flash", ContextLength: 1000000}},
	})

	withOverride := *currentFeatureRuntime()
	withOverride.modelContext = map[string]int64{"deepseek-v4.1-flash": 600000}
	runtime.commitFeatureRuntime(&withOverride)

	// Selecting a tier must reach the existing snapshot immediately.
	snap := runtime.snapshotForAuthID("auth-commit")
	if len(snap.Models) != 1 || snap.Models[0].ContextLength != 600000 {
		t.Fatalf("after set, snapshot context = %+v, want 600000", snap.Models)
	}

	cleared := *currentFeatureRuntime()
	cleared.modelContext = nil
	runtime.commitFeatureRuntime(&cleared)

	snap = runtime.snapshotForAuthID("auth-commit")
	if len(snap.Models) != 1 || snap.Models[0].ContextLength != 1000000 {
		t.Fatalf("after clear, snapshot context = %+v, want rollback to 1000000", snap.Models)
	}
}

// Clearing an override must roll the value back in snapshots that already baked
// the override in. Without this, "auto" left the old tier visible to clients.
func TestResetModelContextSnapshotRestoresBaseLength(t *testing.T) {
	resetModelContextCatalog(t)
	withModelContextOverrides(t, nil)
	recordModelContextFacts([]modelFacts{{
		ID:                      "deepseek-v4.1-flash",
		MaxInputTokens:          ptrInt64(1000000),
		DefaultContextLength:    ptrInt64(300000),
		SupportedContextLengths: []int64{300000, 600000, 1000000},
	}})

	oldRuntime := activeModelRuntime.Load()
	runtime := newModelRuntime(newModelStore(t.TempDir()), func(*http.Request, string) (*hostHTTPResponse, error) {
		return nil, nil
	})
	activeModelRuntime.Store(runtime)
	oldList := panelHostAuthList
	panelHostAuthList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{{ID: "auth-reset", AuthIndex: "idx-reset"}}, nil
	}
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		panelHostAuthList = oldList
	})

	slot := runtime.authSlot("auth-reset")
	storeModelReadinessSnapshot(slot, modelReadinessSnapshot{
		State:       modelReady,
		ModelSource: modelSourceFresh,
		Models:      []pluginapi.ModelInfo{{ID: "deepseek-v4.1-flash", ContextLength: 600000}},
	})
	// Baseline is recorded the way discovery does it: from the untouched value.
	recordBaseContextLength("deepseek-v4.1-flash", 1000000)

	if !runtime.reapplyModelContextSnapshots("deepseek-v4.1-flash") {
		t.Fatal("reset reported no change for an override-baked snapshot")
	}
	snap := runtime.snapshotForAuthID("auth-reset")
	if len(snap.Models) != 1 || snap.Models[0].ContextLength != 1000000 {
		t.Fatalf("snapshot context = %+v, want 1000000", snap.Models)
	}
}
