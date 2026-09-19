package main

import "testing"

func TestClassifyModelID_ShapeRules(t *testing.T) {
	cases := []struct {
		id     string
		kind   modelKind
		usable bool
	}{
		{id: "serve-chat", kind: modelKindChat, usable: true},
		{id: "fast-model", kind: modelKindChat, usable: true},
		{id: "codewise-rewrite", kind: modelKindInline, usable: true},
		{id: "codewise-jump", kind: modelKindInline, usable: true},
		{id: "completion-gf", kind: modelKindCompletion, usable: false},
		{id: "something-taco-completion", kind: modelKindCompletion, usable: false},
		{id: "hunyuan-image-alpha", kind: modelKindImage, usable: false},
		{id: "hunyuan-image-alpha-edit", kind: modelKindImage, usable: false},
		{id: "default-1.1", kind: modelKindUnknown, usable: false},
		{id: "default-model", kind: modelKindUnknown, usable: false},
		{id: "primary-model", kind: modelKindUnknown, usable: false},
		{id: "lite", kind: modelKindUnknown, usable: false},
		{id: "tiny-3b", kind: modelKindUnknown, usable: false},
		{id: "dense-7b-dense", kind: modelKindUnknown, usable: false},
		{id: "", kind: modelKindUnknown, usable: false},
	}
	for _, c := range cases {
		got := classifyModelID(c.id)
		if got != c.kind {
			t.Errorf("classifyModelID(%q) = %q, want %q", c.id, got, c.kind)
		}
		if usable := modelKindChatUsable(got); usable != c.usable {
			t.Errorf("modelKindChatUsable(%q) = %v, want %v", got, usable, c.usable)
		}
	}
}

// The classifier is case-insensitive: upstream IDs are lowercase but an
// operator-added or aliased entry need not be.
func TestClassifyModelID_CaseInsensitive(t *testing.T) {
	if got := classifyModelID("  HUNYUAN-IMAGE-ALPHA  "); got != modelKindImage {
		t.Fatalf("got %q, want image", got)
	}
}

// An unrecognised model must stay visible as chat rather than be dropped:
// missing from the list is a worse failure than wrongly labelled.
func TestClassifyModelID_UnknownShapeDefaultsToChat(t *testing.T) {
	if got := classifyModelID("brand-new-model-2027"); got != modelKindChat {
		t.Fatalf("got %q, want chat", got)
	}
}

func TestModelKindLabel_HasLabelForEveryKind(t *testing.T) {
	for _, kind := range []modelKind{modelKindChat, modelKindCompletion, modelKindInline, modelKindImage, modelKindUnknown} {
		if modelKindLabel(kind) == "" {
			t.Errorf("no label for %q", kind)
		}
	}
	if modelKindLabel(modelKind("nonsense")) != string(modelKindUnknown) {
		t.Errorf("unknown kind should fall back, got %q", modelKindLabel(modelKind("nonsense")))
	}
}

// TestUpgradeModelID_PrefersConfiguredConcrete: when a preset has a concrete
// model configured and present, the served ID is that model.
func TestUpgradeModelID_PrefersConfiguredConcrete(t *testing.T) {
	restore := setPresetTargetsForTest(map[string][]string{"preset-a": {"concrete-a", "concrete-b"}})
	defer restore()
	present := map[string]struct{}{"preset-a": {}, "concrete-a": {}}
	if got := upgradeModelID("preset-a", present); got != "concrete-a" {
		t.Fatalf("got %q, want concrete-a", got)
	}
	// First configured target wins when several are present.
	both := map[string]struct{}{"concrete-a": {}, "concrete-b": {}}
	if got := upgradeModelID("preset-a", both); got != "concrete-a" {
		t.Fatalf("got %q, want the first configured target", got)
	}
}

// A preset with no configured target must be served as itself.
func TestUpgradeModelID_KeepsAliasWhenNoTarget(t *testing.T) {
	if got := upgradeModelID("preset-a", map[string]struct{}{"preset-a": {}}); got != "preset-a" {
		t.Fatalf("got %q, want the alias itself", got)
	}
	if got := upgradeModelID("serve-chat", nil); got != "serve-chat" {
		t.Fatalf("a normal ID must pass through, got %q", got)
	}
	if got := upgradeModelID("", nil); got != "" {
		t.Fatalf("blank must stay blank, got %q", got)
	}
}

// The upgrade is case-insensitive so a hand-added alias still upgrades.
func TestUpgradeModelID_CaseInsensitive(t *testing.T) {
	restore := setPresetTargetsForTest(map[string][]string{"preset-a": {"concrete-a"}})
	defer restore()
	if got := upgradeModelID("  PRESET-A ", map[string]struct{}{"concrete-a": {}}); got != "concrete-a" {
		t.Fatalf("got %q, want concrete-a", got)
	}
}

// Complete is additive and idempotent: running it twice must not grow the list.
func TestCompletePresetModels_Idempotent(t *testing.T) {
	restore := setPresetTargetsForTest(map[string][]string{"preset-a": {"concrete-a"}})
	defer restore()
	once := completePresetModels([]string{"preset-a", "serve-chat"})
	twice := completePresetModels(once)
	if len(once) != 3 || len(twice) != 3 {
		t.Fatalf("complete must be idempotent, got %#v then %#v", once, twice)
	}
}

func TestServedPresetAlias_NamesTheAlias(t *testing.T) {
	restore := setPresetTargetsForTest(map[string][]string{"preset-a": {"concrete-a"}})
	defer restore()
	if got := servedPresetAlias("concrete-a"); got != "preset-a" {
		t.Fatalf("got %q, want preset-a", got)
	}
	if got := servedPresetAlias("serve-chat"); got != "" {
		t.Fatalf("a non-preset ID should have no alias, got %q", got)
	}
}

// The mapping must come from config, not from code: production files are
// contract-tested against hard-coded model IDs, so a preset alias is only
// resolved when the operator configured it.
func TestParseFeatureRuntime_PresetModels(t *testing.T) {
	cfg, err := parseFeatureRuntime([]byte("preset_models:\n  preset-a: concrete-a\n  preset-b: [concrete-b, concrete-c]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.presetTargets["preset-a"]; len(got) != 1 || got[0] != "concrete-a" {
		t.Fatalf("preset-a = %#v", got)
	}
	if got := cfg.presetTargets["preset-b"]; len(got) != 2 || got[0] != "concrete-b" {
		t.Fatalf("preset-b = %#v", got)
	}
}

func TestParseFeatureRuntime_PresetModelsRejectsBadShape(t *testing.T) {
	bad := []string{
		"preset_models: [a]\n",
		"preset_models:\n  preset-a:\n",
		"preset_models:\n  preset-a: []\n",
		"preset_models:\n  preset-a: [x, x]\n",
		"preset_models:\n  \"\": x\n",
		"preset_models:\n  preset-a: {k: v}\n",
	}
	for _, raw := range bad {
		if _, err := parseFeatureRuntime([]byte(raw)); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

// Unset means "serve the alias as itself" — no guessing.
func TestParseFeatureRuntime_PresetModelsUnset(t *testing.T) {
	cfg, err := parseFeatureRuntime(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.presetTargets) != 0 {
		t.Fatalf("unset preset_models should be empty, got %#v", cfg.presetTargets)
	}
	if got := servedPresetAlias("preset-a"); got != "" {
		t.Fatalf("no mapping means no alias, got %q", got)
	}
}

// Completion is a no-op when entitlement already carries a concrete model:
// `present` is the entitlement list, so this is decided before the rich
// catalog is merged in.
func TestCompletePresetModels_NoopWhenEntitled(t *testing.T) {
	restore := setPresetTargetsForTest(map[string][]string{"preset-a": {"concrete-a", "concrete-b"}})
	defer restore()
	got := completePresetModels([]string{"preset-a", "concrete-b"})
	if len(got) != 2 || got[0] != "preset-a" || got[1] != "concrete-b" {
		t.Fatalf("entitled concrete model should make completion a no-op, got %#v", got)
	}
}
