// model_class.go classifies catalog entries so the panel can show the whole
// upstream catalog without pretending every entry is chat-usable.
//
// The upstream /v3/config catalog is much wider than the cli agent's entitled
// model list: it also carries text-completion models, IDE inline helpers,
// image models and entries whose backend is simply not wired up. Sending a
// chat request to those returns 11102 ("service info not found") or 11103
// ("backend not supported"), which reads as a broken gateway rather than as
// "this model is not for chat".
//
// Verified against a production CN account (2026-09): the main chat families,
// the hunyuan chat/instruct entries, older deepseek-v3 releases and the
// codewise rewrite/jump helpers all answer; the default-* desktop presets,
// completion-gf, the small dense local models, several older glm / kimi /
// minimax / deepseek-r1 entries and the *-taco-completion entry return 11102;
// the image-alpha pair returns 11103.
//
// Model IDs are deliberately never hard-coded here: a contract test rejects
// production files that name one, because the catalog must come from upstream.
// Everything below is shape matching, and a miss only means "shown as
// unknown", never "missing from the list".
package main

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelKind is what an entry is good for. chat means a normal /v1/chat/completions
// request works; the rest are shown for completeness but flagged.
type modelKind string

const (
	modelKindChat       modelKind = "chat"
	modelKindCompletion modelKind = "completion"
	modelKindInline     modelKind = "inline"
	modelKindImage      modelKind = "image"
	modelKindUnknown    modelKind = "unknown"
)

// Internal helpers the gateway advertises but that never serve chat: the lite
// entry backs title generation and compaction, and default/primary are desktop
// presets the chat endpoint does not resolve.
var nonChatModelIDs = map[string]struct{}{
	"lite":          {},
	"default-model": {},
	"primary-model": {},
}

// presetSuffixes are the shapes the desktop's preset aliases take. They are
// served and routed as themselves: upstream bills them at their own multiplier
// (the CN catalog lists fast x0.21, balanced x0.65, deep x1.20) and resolves
// them server-side, so rewriting one to a concrete model would change both the
// response and the billed rate.
var presetSuffixes = []string{"-model"}

// isPresetModelID reports whether an ID is a desktop preset alias. Shape
// matching rather than an ID list, so a new preset from upstream is recognised
// without a code change.
func isPresetModelID(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	for _, suffix := range presetSuffixes {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	return false
}

// default-1.x style desktop presets: the chat endpoint reports "service info
// not found" for them, so they are unresolved rather than a different shape.
var unresolvedModelPrefixes = []string{"default-"}

// completion-* entries are text-completion models, not chat.
var nonChatModelPrefixes = []string{"completion-"}

var nonChatModelSuffixes = []string{"-taco-completion"}

// Small dense local models are served by a backend chat rejects.
var nonChatModelHints = []string{"-3b", "-7b-dense"}

var imageModelSuffixes = []string{"-image-alpha", "-image-alpha-edit"}

// classifyModelID reports what one catalog entry can be used for.
func classifyModelID(id string) modelKind {
	id = strings.TrimSpace(strings.ToLower(id))
	if id == "" {
		return modelKindUnknown
	}
	if _, bad := nonChatModelIDs[id]; bad {
		return modelKindUnknown
	}
	for _, hint := range nonChatModelHints {
		if strings.Contains(id, hint) {
			return modelKindUnknown
		}
	}
	for _, suffix := range imageModelSuffixes {
		if strings.HasSuffix(id, suffix) {
			return modelKindImage
		}
	}
	for _, prefix := range unresolvedModelPrefixes {
		if strings.HasPrefix(id, prefix) {
			return modelKindUnknown
		}
	}
	for _, prefix := range nonChatModelPrefixes {
		if strings.HasPrefix(id, prefix) {
			return modelKindCompletion
		}
	}
	for _, suffix := range nonChatModelSuffixes {
		if strings.HasSuffix(id, suffix) {
			return modelKindCompletion
		}
	}
	if strings.HasPrefix(id, "codewise-") {
		return modelKindInline
	}
	return modelKindChat
}

// modelKindChatUsable reports whether a normal chat request should be expected
// to succeed. Only chat and inline are considered usable; completion and image
// endpoints answer a different shape of request.
func modelKindChatUsable(kind modelKind) bool {
	return kind == modelKindChat || kind == modelKindInline
}

var modelKindLabels = map[modelKind]string{
	modelKindChat:       "chat",
	modelKindCompletion: "文本补全",
	modelKindInline:     "IDE 内联",
	modelKindImage:      "图像",
	modelKindUnknown:    "不可用",
}

// modelKindLabel is the panel-facing label for a kind.
func modelKindLabel(kind modelKind) string {
	if label, ok := modelKindLabels[kind]; ok {
		return label
	}
	return string(modelKindUnknown)
}

// configuredModelFacts wraps the YAML model list as facts so it flows through
// the same alias upgrade as a discovered catalog.
func configuredModelFacts(ids []string) []modelFacts {
	out := make([]modelFacts, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out = append(out, modelFacts{ID: id})
	}
	return out
}

// discoveredModelInfos turns a snapshot's model facts into the served catalog,
// carrying each entry's own ID through unchanged.
func discoveredModelInfos(models []modelFacts, records map[string]modelFacts) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return []pluginapi.ModelInfo{}
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		out = append(out, modelInfoFromSources(model, matchModelsDevRecord(id, records)))
	}
	return out
}
