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

// A preset carries its own upstream credits and capabilities, so it is
// forwarded as itself: rewriting it to some concrete model behind it would
// change both the response and the billed rate.
func TestResolveUpstreamModel_KeepsPresetAlias(t *testing.T) {
	if got := resolveUpstreamModel("fast-model", nil); got != "fast-model" {
		t.Fatalf("got %q, want the alias itself", got)
	}
	// A host-supplied oauth-model-alias still wins: it is an explicit override.
	defer setModelAliasForTest(map[string]string{"fast-model": "host-target"})()
	if got := resolveUpstreamModel("fast-model", nil); got != "host-target" {
		t.Fatalf("host alias should win, got %q", got)
	}
	if got := resolveUpstreamModel("serve-chat", nil); got != "serve-chat" {
		t.Fatalf("got %q, want serve-chat", got)
	}
}

// The served catalog must carry each entry's own ID through unchanged. The
// presets are real upstream models, not aliases to be rewritten.
func TestDiscoveredModelInfosKeepsIDs(t *testing.T) {
	got := discoveredModelInfos([]modelFacts{{ID: "preset-a"}, {ID: "concrete-a"}}, nil)
	if len(got) != 2 || got[0].ID != "preset-a" || got[1].ID != "concrete-a" {
		t.Fatalf("catalog IDs must pass through unchanged, got %#v", got)
	}
}

// isPresetModelID is shape matching, so a preset the plugin has never seen is
// still recognised as one.
func TestIsPresetModelID_ShapeMatching(t *testing.T) {
	for _, id := range []string{"fast-model", "balanced-model", "deep-model", "some-new-model"} {
		if !isPresetModelID(id) {
			t.Errorf("isPresetModelID(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"hy3", "auto", "default", "", "model"} {
		if isPresetModelID(id) {
			t.Errorf("isPresetModelID(%q) = true, want false", id)
		}
	}
}

// setModelAliasForTest installs host-style aliases and returns a restore func.
func setModelAliasForTest(byAlias map[string]string) func() {
	modelAliasCache.Lock()
	prev := modelAliasCache.byAlias
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
	return func() {
		modelAliasCache.Lock()
		modelAliasCache.byAlias = prev
		modelAliasCache.Unlock()
	}
}
