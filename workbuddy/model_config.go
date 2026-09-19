// model_config.go implements the plugin-owned model list overlay.
//
// The plugin's YAML `models` setting is either empty (dynamic discovery) or an
// authoritative full catalog. That is enough to replace the catalog wholesale
// but not to tweak the discovered one. This file adds an in-memory overlay
// applied on top of whatever the base catalog is:
//
//	hide  - drop listed model IDs from the effective list
//	order - pin listed model IDs to the front, in the given order
//	add   - append synthetic model IDs that upstream never reported
//
// The overlay is process-local: it survives config reloads but not a CPA
// restart. It is intentionally separate from the YAML `models` setting so an
// operator can keep upstream discovery and still curate the result.
package main

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// modelOverlay is the plugin-owned curation applied to the base catalog.
type modelOverlay struct {
	Hide  []string `json:"hide"`
	Order []string `json:"order"`
	Add   []string `json:"add"`
}

// modelOverlayState carries the overlay plus the bookkeeping the UI needs.
type modelOverlayState struct {
	Overlay modelOverlay `json:"overlay"`
	// Revision bumps on every successful write so callers can detect races.
	Revision int64 `json:"revision"`
}

var (
	overlayMu     sync.RWMutex
	overlayState  = modelOverlayState{}
	modelIDMaxLen = maxDiscoveredModelIDBytes
)

// setModelOverlayForTest installs an overlay and returns a restore func.
func setModelOverlayForTest(o modelOverlay) func() {
	overlayMu.Lock()
	prev := overlayState
	overlayState.Overlay = o
	overlayState.Revision++
	overlayMu.Unlock()
	return func() {
		overlayMu.Lock()
		overlayState = prev
		overlayMu.Unlock()
	}
}

// loadedModelOverlay returns a copy of the current overlay plus its revision.
func loadedModelOverlay() (modelOverlay, int64) {
	overlayMu.RLock()
	defer overlayMu.RUnlock()
	o := overlayState.Overlay
	return modelOverlay{
		Hide:  append([]string(nil), o.Hide...),
		Order: append([]string(nil), o.Order...),
		Add:   append([]string(nil), o.Add...),
	}, overlayState.Revision
}

// loadedModelOverlayForRead returns just the overlay for catalog assembly.
func loadedModelOverlayForRead() modelOverlay {
	o, _ := loadedModelOverlay()
	return o
}

// normalizeModelIDList validates and trims a list of model IDs. Empty entries
// are rejected because a blank ID would silently match nothing (or everything).
func normalizeModelIDList(in []string, field string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, &modelConfigError{field: field, msg: "entries must not be empty"}
		}
		if strings.IndexFunc(id, func(r rune) bool {
			return r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
		}) >= 0 {
			return nil, &modelConfigError{field: field, msg: "entries must be single-line strings"}
		}
		if len(id) > modelIDMaxLen {
			return nil, &modelConfigError{field: field, msg: "entry exceeds maximum ID length"}
		}
		if _, exists := seen[id]; exists {
			return nil, &modelConfigError{field: field, msg: "entries must not be duplicated"}
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

type modelConfigError struct {
	field string
	msg   string
}

func (e *modelConfigError) Error() string {
	if e.field == "" {
		return e.msg
	}
	return e.field + ": " + e.msg
}

// storeModelOverlay validates then commits an overlay. The previous value is
// returned unchanged when validation fails.
func storeModelOverlay(next modelOverlay) (modelOverlayState, error) {
	hide, err := normalizeModelIDList(next.Hide, "hide")
	if err != nil {
		return modelOverlayState{}, err
	}
	order, err := normalizeModelIDList(next.Order, "order")
	if err != nil {
		return modelOverlayState{}, err
	}
	add, err := normalizeModelIDList(next.Add, "add")
	if err != nil {
		return modelOverlayState{}, err
	}
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlayState.Overlay = modelOverlay{Hide: hide, Order: order, Add: add}
	overlayState.Revision++
	return modelOverlayState{
		Overlay: modelOverlay{
			Hide:  append([]string(nil), hide...),
			Order: append([]string(nil), order...),
			Add:   append([]string(nil), add...),
		},
		Revision: overlayState.Revision,
	}, nil
}

// applyModelOverlay applies hide/order/add to a base catalog. The input slice
// is never mutated. Ordering is stable: pinned IDs come first in the operator's
// order, remaining base IDs keep their original relative order, then additions.
func applyModelOverlay(base []pluginapi.ModelInfo, o modelOverlay) []pluginapi.ModelInfo {
	hidden := make(map[string]struct{}, len(o.Hide))
	for _, id := range o.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}

	pinned := make(map[string]int, len(o.Order))
	for i, id := range o.Order {
		id = strings.TrimSpace(id)
		if _, dup := pinned[id]; dup {
			continue
		}
		pinned[id] = i
	}

	kept := make([]pluginapi.ModelInfo, 0, len(base)+len(o.Add))
	pinnedOut := make([]pluginapi.ModelInfo, 0, len(o.Order))
	for _, m := range base {
		if _, gone := hidden[strings.TrimSpace(m.ID)]; gone {
			continue
		}
		if _, isPinned := pinned[strings.TrimSpace(m.ID)]; isPinned {
			pinnedOut = append(pinnedOut, m)
			continue
		}
		kept = append(kept, m)
	}

	sort.SliceStable(pinnedOut, func(i, j int) bool {
		return pinned[strings.TrimSpace(pinnedOut[i].ID)] < pinned[strings.TrimSpace(pinnedOut[j].ID)]
	})

	out := make([]pluginapi.ModelInfo, 0, len(pinnedOut)+len(kept)+len(o.Add))
	out = append(out, pinnedOut...)
	out = append(out, kept...)
	for _, id := range o.Add {
		id = strings.TrimSpace(id)
		if _, gone := hidden[id]; gone {
			continue
		}
		out = append(out, defaultModelInfo(id, ""))
	}
	return out
}

// applyModelOverlayForAdmin applies the overlay but keeps hidden entries in the
// result. The management panel needs to see them so an operator can undo a
// hide; the serving model capability still uses applyModelOverlay so a hidden
// entry is not advertised to clients.
func applyModelOverlayForAdmin(base []pluginapi.ModelInfo, o modelOverlay) []pluginapi.ModelInfo {
	visible := applyModelOverlay(base, o)
	hidden := make(map[string]struct{}, len(o.Hide))
	for _, id := range o.Hide {
		hidden[strings.TrimSpace(id)] = struct{}{}
	}
	if len(hidden) == 0 {
		return visible
	}

	seen := make(map[string]struct{}, len(visible)+len(hidden))
	out := make([]pluginapi.ModelInfo, 0, len(visible)+len(hidden))
	for _, model := range visible {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	// Preserve the base catalog order for hidden entries. Appending them at the
	// end would jump rows around as soon as an operator restores one.
	for _, model := range base {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, isHidden := hidden[id]; !isHidden {
			continue
		}
		if _, already := seen[id]; already {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, model)
	}
	return out
}

// effectiveModelCatalog unions the per-account readiness snapshots into one
// catalog, then applies the overlay. Accounts that never reported models
// contribute nothing, which is why an empty result is not an error.
func effectiveModelCatalog() ([]pluginapi.ModelInfo, string) {
	models, source := effectiveModelCatalogTyped()
	return models, string(source)
}

func effectiveModelCatalogTyped() ([]pluginapi.ModelInfo, modelSnapshotSource) {
	models, source, overlay := baseModelCatalogTyped()
	return applyModelOverlay(models, overlay), source
}

// adminModelCatalogTyped is the same base catalog as effectiveModelCatalogTyped,
// but keeps hidden entries so the management panel can list and restore them.
func adminModelCatalogTyped() ([]pluginapi.ModelInfo, modelSnapshotSource) {
	models, source, overlay := baseModelCatalogTyped()
	return applyModelOverlayForAdmin(models, overlay), source
}

// baseModelCatalogTyped unions the per-account readiness snapshots into one
// catalog without applying the overlay.
func baseModelCatalogTyped() ([]pluginapi.ModelInfo, modelSnapshotSource, modelOverlay) {
	runtime := activeModelRuntime.Load()
	seen := make(map[string]struct{})
	source := modelSourceNone
	out := make([]pluginapi.ModelInfo, 0)
	if runtime != nil {
		for _, file := range panelModelAuthFiles() {
			snap := runtime.snapshotForAuthID(file.ID)
			for _, m := range snap.Models {
				id := strings.TrimSpace(m.ID)
				if id == "" {
					continue
				}
				if _, dup := seen[id]; dup {
					continue
				}
				seen[id] = struct{}{}
				out = append(out, m)
			}
			// Prefer the most authoritative source seen: config > fresh > cache.
			if modelSourceRank(snap.ModelSource) > modelSourceRank(source) {
				source = snap.ModelSource
			}
		}
	}
	if len(out) == 0 {
		if cfg := currentFeatureRuntime(); cfg != nil && len(cfg.configuredModels) > 0 {
			source = modelSourceConfig
			for _, id := range cfg.configuredModels {
				out = append(out, defaultModelInfo(id, ""))
			}
		}
	}
	return out, source, loadedModelOverlayForRead()
}

// panelModelAuthFiles lists auth files for catalog aggregation. Falls back to
// an empty slice when the host is unavailable so callers degrade gracefully.
func panelModelAuthFiles() []pluginapi.HostAuthFileEntry {
	files, err := panelHostAuthList()
	if err != nil {
		return nil
	}
	return files
}

// modelSourceRank orders catalog sources by how authoritative they are.
func modelSourceRank(source modelSnapshotSource) int {
	switch source {
	case modelSourceConfig:
		return 3
	case modelSourceFresh:
		return 2
	case modelSourceCache:
		return 1
	default:
		return 0
	}
}

// handleModelListQuery reports the effective catalog, its source, the active
// overlay and each model's cooldown state.
func handleModelListQuery() map[string]any {
	models, source := adminModelCatalogTyped()
	overlay, revision := loadedModelOverlay()
	items := make([]map[string]any, 0, len(models))
	for i, m := range models {
		id := strings.TrimSpace(m.ID)
		items = append(items, map[string]any{
			"id":          id,
			"name":        m.Name,
			"displayName": m.DisplayName,
			"position":    i,
			"hidden":      overlayHidden(overlay, id),
			"custom":      overlayAdded(overlay, id),
			"credits":     modelCreditsField(id),
			// The catalog is wider than what serves chat: completion, image and
			// unresolved entries are listed but flagged so an operator can see
			// them without sending a request that will 11102/11103.
			"kind":       string(classifyModelID(id)),
			"kindLabel":  modelKindLabel(classifyModelID(id)),
			"chatUsable": modelKindChatUsable(classifyModelID(id)),
			// A preset is listed as itself; flag it so an operator knows it is
			// a desktop alias rather than a concrete upstream model.
			"preset": isPresetModelID(id),
			// Throttling is per (account, model), so the catalog-level view can
			// only report how many accounts are affected by this model.
			"coolingAccounts": cooldownModelCount(id),
		})
	}
	configured := []string(nil)
	if cfg := currentFeatureRuntime(); cfg != nil {
		configured = append([]string(nil), cfg.configuredModels...)
	}
	return map[string]any{
		"models":           items,
		"count":            len(items),
		"source":           source,
		"configuredModels": configured,
		"overlay":          overlay,
		"revision":         revision,
		"persistent":       false,
	}
}

func overlayHidden(o modelOverlay, id string) bool {
	for _, h := range o.Hide {
		if strings.TrimSpace(h) == id {
			return true
		}
	}
	return false
}

func overlayAdded(o modelOverlay, id string) bool {
	for _, a := range o.Add {
		if strings.TrimSpace(a) == id {
			return true
		}
	}
	return false
}

// handleModelCreditsQuery reports every known multiplier with its source.
func handleModelCreditsQuery() map[string]any {
	entries := modelCreditsSnapshot()
	return map[string]any{
		"credits":    entries,
		"count":      len(entries),
		"persistent": false,
	}
}

// handleModelCreditsWrite pins or clears one multiplier. An empty value clears
// the pin so the upstream number takes over again.
func handleModelCreditsWrite(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Model   string `json:"model"`
		ID      string `json:"id"`
		Credits string `json:"credits"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	id := strings.TrimSpace(body.Model)
	if id == "" {
		id = strings.TrimSpace(body.ID)
	}
	if id == "" {
		return map[string]any{"success": false, "error": "model is required"}
	}
	value := normalizeModelCredits(body.Credits)
	setModelCreditsOverride(id, value)
	stored, source := modelCredits(id)
	return map[string]any{
		"success":    true,
		"model":      id,
		"credits":    stored,
		"rate":       parseModelCreditsRate(stored),
		"source":     source,
		"persistent": false,
	}
}

// handleModelOverlayWrite replaces the whole overlay.
func handleModelOverlayWrite(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Overlay *modelOverlay `json:"overlay"`
		// Accept the fields at the top level too for convenience.
		Hide  []string `json:"hide"`
		Order []string `json:"order"`
		Add   []string `json:"add"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	next := modelOverlay{Hide: body.Hide, Order: body.Order, Add: body.Add}
	if body.Overlay != nil {
		next = *body.Overlay
	}
	stored, err := storeModelOverlay(next)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{
		"success":    true,
		"overlay":    stored.Overlay,
		"revision":   stored.Revision,
		"persistent": false,
	}
}

// handleModelOverlayAction applies one targeted edit: hide, restore, move or add.
func handleModelOverlayAction(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		IDs    string `json:"ids"`
		// move: offset -1 up, +1 down.
		Offset int `json:"offset"`
	}
	if len(req.Body) > 0 {
		if err := json.Unmarshal(req.Body, &body); err != nil {
			return map[string]any{"success": false, "error": "invalid json body"}
		}
	}
	current, _ := loadedModelOverlay()
	id := strings.TrimSpace(body.ID)
	if id == "" {
		id = strings.TrimSpace(body.IDs)
	}
	switch strings.ToLower(strings.TrimSpace(body.Action)) {
	case "hide":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		if !overlayHidden(current, id) {
			current.Hide = append(current.Hide, id)
		}
		// Hiding wins over any prior pin or synthetic addition.
		current.Order = removeModelID(current.Order, id)
		current.Add = removeModelID(current.Add, id)
	case "restore":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		current.Hide = removeModelID(current.Hide, id)
	case "move":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		var reorderErr error
		current.Order, reorderErr = moveModelID(current.Order, id, body.Offset)
		if reorderErr != nil {
			return map[string]any{"success": false, "error": reorderErr.Error()}
		}
	case "add":
		if id == "" {
			return map[string]any{"success": false, "error": "id is required"}
		}
		if !overlayAdded(current, id) {
			current.Add = append(current.Add, id)
		}
		current.Hide = removeModelID(current.Hide, id)
	default:
		return map[string]any{"success": false, "error": "action must be hide, restore, move or add"}
	}
	stored, err := storeModelOverlay(current)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	return map[string]any{
		"success":    true,
		"overlay":    stored.Overlay,
		"revision":   stored.Revision,
		"persistent": false,
	}
}

func removeModelID(list []string, id string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if strings.TrimSpace(v) == id {
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// moveModelID shifts one ID within the pin list, seeding the list from its
// current position when it is not pinned yet.
func moveModelID(list []string, id string, offset int) ([]string, error) {
	if offset != -1 && offset != 1 {
		return nil, &modelConfigError{field: "offset", msg: "must be -1 (up) or 1 (down)"}
	}
	idx := -1
	for i, v := range list {
		if strings.TrimSpace(v) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		list = append(append([]string(nil), list...), id)
		idx = len(list) - 1
	}
	target := idx + offset
	if target < 0 || target >= len(list) {
		return list, nil
	}
	out := append([]string(nil), list...)
	out[idx], out[target] = out[target], out[idx]
	return out, nil
}
