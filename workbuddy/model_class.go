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
	"sync"

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
//
// "default" is excluded from that list on purpose. The CN cli agent's
// entitlement carries "auto" as its entry for the default model, and the
// plugin's own static list names it "auto"; upstream resolves it as "default".
// Both spellings therefore have to reach the gateway rather than be filtered
// as a desktop preset.
var nonChatModelIDs = map[string]struct{}{
	"lite":          {},
	"default-model": {},
	"primary-model": {},
}

// presetModelAliases are the desktop's fast/balanced/deep style presets. They
// are chat-usable in the app — the desktop resolves them client-side — so they
// are served, and the served ID is upgraded to the concrete model behind them.
//
// The alias set is shape-matched rather than enumerated so a new preset from
// upstream is recognised without a code change.
// presetSuffixes are the shapes desktop preset aliases take.
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

// presetTargets is the operator-owned mapping from a preset alias to the
// concrete model IDs behind it. It is empty by default, which means "serve the
// alias as itself": the plugin never guesses which concrete model a preset
// resolves to, because guessing silently changes both the response and the
// billed multiplier.
var (
	presetTargetsMu sync.RWMutex
	presetTargets   = map[string][]string{}
)

// setPresetTargetsForTest installs a preset mapping and returns a restore func.
func setPresetTargetsForTest(targets map[string][]string) func() {
	presetTargetsMu.Lock()
	prev := presetTargets
	presetTargets = targets
	presetTargetsMu.Unlock()
	return func() {
		presetTargetsMu.Lock()
		presetTargets = prev
		presetTargetsMu.Unlock()
	}
}

func loadedPresetTargets() map[string][]string {
	presetTargetsMu.RLock()
	defer presetTargetsMu.RUnlock()
	if len(presetTargets) == 0 {
		return nil
	}
	out := make(map[string][]string, len(presetTargets))
	for alias, ids := range presetTargets {
		out[alias] = append([]string(nil), ids...)
	}
	return out
}

func anyPresent(ids []string, present map[string]struct{}) bool {
	for _, id := range ids {
		if _, exists := present[id]; exists {
			return true
		}
	}
	return false
}

// completePresetModels appends the concrete model behind any entitled preset
// that the catalog is otherwise missing, so an account entitled to a preset is
// not silently served a shorter catalog. It is additive and never invents a
// model: a preset with no configured concrete model keeps just the preset.
func completePresetModels(ids []string) []string {
	targets := loadedPresetTargets()
	if len(ids) == 0 || len(targets) == 0 {
		return ids
	}
	present := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		present[strings.TrimSpace(id)] = struct{}{}
	}
	out := append([]string(nil), ids...)
	for _, id := range ids {
		alias := strings.ToLower(strings.TrimSpace(id))
		candidates, ok := targets[alias]
		if !ok {
			continue
		}
		// The preset is already backed by one of its concrete models, so there
		// is nothing to complete. Adding another would grow the catalog for no
		// routing benefit.
		if anyPresent(candidates, present) {
			continue
		}
		first := candidates[0]
		present[first] = struct{}{}
		out = append(out, first)
	}
	return out
}

// upgradeModelID is retained for callers that request the rename explicitly.
// The catalog no longer applies it: a preset carries its own upstream credits
// and capabilities, so rewriting it to the concrete model behind it would
// change both the response and the billed rate. completePresetModels is what
// widens the catalog.
func upgradeModelID(id string, present map[string]struct{}) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for _, candidate := range loadedPresetTargets()[strings.ToLower(id)] {
		if _, exists := present[candidate]; exists {
			return candidate
		}
	}
	return id
}

// upgradeModelIDs applies upgradeModelID to a whole catalog.
func upgradeModelIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		present[strings.TrimSpace(id)] = struct{}{}
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, upgradeModelID(id, present))
	}
	return out
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

// discoveredModelInfos turns a snapshot's model facts into the served catalog.
// Preset aliases are upgraded in place here so every consumer — the host's
// model list, the panel and routing — sees the same concrete IDs.
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

// servedPresetAlias reports the preset alias whose upgrade produces id, or ""
// when the ID is served as itself.
func servedPresetAlias(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for alias, candidates := range loadedPresetTargets() {
		for _, candidate := range candidates {
			if candidate == id {
				return alias
			}
		}
	}
	return ""
}

// setPresetTargets publishes the operator-configured preset mapping. It is the
// only writer: config is the single source, and callers read a copy.
func setPresetTargets(targets map[string][]string) {
	presetTargetsMu.Lock()
	defer presetTargetsMu.Unlock()
	if len(targets) == 0 {
		presetTargets = map[string][]string{}
		return
	}
	next := make(map[string][]string, len(targets))
	for alias, ids := range targets {
		next[alias] = append([]string(nil), ids...)
	}
	presetTargets = next
}

// presetTargetFor returns the concrete model a preset alias is forwarded to,
// or the ID itself when no mapping is configured. Unlike upgradeModelID it does
// not require the target to be present in a catalog: callers that have no
// catalog in hand (the executor) rely on downstream validation instead.
func presetTargetFor(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	targets := loadedPresetTargets()[strings.ToLower(id)]
	if len(targets) == 0 {
		return id
	}
	return targets[0]
}

// canonicalModelID folds the desktop's "auto" entry onto the upstream ID the
// gateway resolves it to. The CN cli agent is entitled to "auto" while upstream
// serves that model as "default", so only one spelling was routable.
func canonicalModelID(id string) string {
	if strings.EqualFold(strings.TrimSpace(id), "auto") {
		return "default"
	}
	return id
}
