package main

import (
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const maxContextWindowValue int64 = 100_000_000

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// contextMetadata is the WorkBuddy /v3/config context-window capability. It is
// kept separately from pluginapi.ModelInfo because the CPA SDK only exposes one
// effective ContextLength, not the provider's selectable window list.
type contextMetadata struct {
	DefaultLength    int64   `json:"default_length,omitempty"`
	SupportedLengths []int64 `json:"supported_lengths,omitempty"`
	MaxAllowedSize   int64   `json:"max_allowed_size,omitempty"`
	MaxInputTokens   int64   `json:"max_input_tokens,omitempty"`
}

var modelContextCatalog = struct {
	sync.RWMutex
	byModel map[string]contextMetadata
	// baseLength keeps the first observed upstream/models.dev value per model.
	// Snapshots bake in the effective value, so without an untouched baseline a
	// cleared override could not be rolled back.
	baseLength map[string]int64
}{byModel: make(map[string]contextMetadata), baseLength: make(map[string]int64)}

// recordBaseContextLength remembers the untouched value the first time a model
// is seen. Later calls never overwrite it.
func recordBaseContextLength(id string, value int64) {
	key := strings.ToLower(strings.TrimSpace(id))
	if key == "" || value <= 0 {
		return
	}
	modelContextCatalog.Lock()
	if _, exists := modelContextCatalog.baseLength[key]; !exists {
		modelContextCatalog.baseLength[key] = value
	}
	modelContextCatalog.Unlock()
}

func baseContextLengthForModel(id string) (int64, bool) {
	modelContextCatalog.RLock()
	value, ok := modelContextCatalog.baseLength[strings.ToLower(strings.TrimSpace(id))]
	modelContextCatalog.RUnlock()
	return value, ok
}

func normalizeContextLengths(values []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(values))
	out := make([]int64, 0, len(values))
	for _, value := range values {
		if value <= 0 || value > maxContextWindowValue {
			return nil, &modelConfigError{field: "context lengths", msg: "must be positive and within the supported maximum"}
		}
		if _, ok := seen[value]; ok {
			return nil, &modelConfigError{field: "context lengths", msg: "must not contain duplicates"}
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func validateContextMetadata(meta contextMetadata) (contextMetadata, error) {
	if meta.DefaultLength < 0 || meta.MaxAllowedSize < 0 || meta.MaxInputTokens < 0 {
		return contextMetadata{}, &modelConfigError{field: "context metadata", msg: "values must not be negative"}
	}
	if meta.DefaultLength > maxContextWindowValue || meta.MaxAllowedSize > maxContextWindowValue || meta.MaxInputTokens > maxContextWindowValue {
		return contextMetadata{}, &modelConfigError{field: "context metadata", msg: "value exceeds maximum"}
	}
	lengths, err := normalizeContextLengths(meta.SupportedLengths)
	if err != nil {
		return contextMetadata{}, err
	}
	// The provider's default tier is data, not policy: a default outside the
	// advertised list (or a legacy numeric contextWindow) must not fail the whole
	// catalog. Drop only the inconsistent default and keep the capability.
	if meta.DefaultLength > 0 && len(lengths) > 0 {
		listed := false
		for _, value := range lengths {
			if value == meta.DefaultLength {
				listed = true
				break
			}
		}
		if !listed {
			lengths = append(lengths, meta.DefaultLength)
			lengths, err = normalizeContextLengths(lengths)
			if err != nil {
				return contextMetadata{}, err
			}
		}
	}
	meta.SupportedLengths = lengths
	return meta, nil
}

func cloneContextMetadata(meta contextMetadata) contextMetadata {
	meta.SupportedLengths = append([]int64(nil), meta.SupportedLengths...)
	return meta
}

func recordModelContextFacts(models []modelFacts) {
	if len(models) == 0 {
		return
	}
	modelContextCatalog.Lock()
	defer modelContextCatalog.Unlock()
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		meta := contextMetadata{
			DefaultLength:    derefInt64(model.DefaultContextLength),
			SupportedLengths: append([]int64(nil), model.SupportedContextLengths...),
			MaxAllowedSize:   derefInt64(model.MaxAllowedSize),
			MaxInputTokens:   derefInt64(model.MaxInputTokens),
		}
		if normalized, err := validateContextMetadata(meta); err == nil {
			modelContextCatalog.byModel[strings.ToLower(id)] = normalized
		}
	}
}

func contextMetadataForModel(id string) (contextMetadata, bool) {
	modelContextCatalog.RLock()
	meta, ok := modelContextCatalog.byModel[strings.ToLower(strings.TrimSpace(id))]
	modelContextCatalog.RUnlock()
	if !ok {
		return contextMetadata{}, false
	}
	return cloneContextMetadata(meta), true
}

func contextOverrideForModel(id string) (int64, bool) {
	cfg := currentFeatureRuntime()
	if cfg == nil || cfg.modelContext == nil {
		return 0, false
	}
	value, ok := cfg.modelContext[strings.TrimSpace(id)]
	return value, ok && value > 0
}

// effectiveModelContext reports the context length CPA should advertise for one
// model, plus where that value came from and the selectable provider tiers.
//
// Without an operator override the advertised value stays the model's maximum
// capability (maxInputTokens / maxAllowedSize). That preserves the previous
// behaviour where a 1M-capable model is not silently advertised as 300K just
// because 300K happens to be the provider's own default tier. An explicit
// override wins, so an operator who picks 300K gets a 300K budget.
func effectiveModelContext(id string, fallback int64) (int64, string, []int64) {
	meta, hasMeta := contextMetadataForModel(id)
	if override, ok := contextOverrideForModel(id); ok {
		return override, "override", append([]int64(nil), meta.SupportedLengths...)
	}
	if hasMeta {
		candidate := fallback
		if candidate <= 0 {
			candidate = meta.MaxInputTokens
		}
		if candidate <= 0 {
			candidate = meta.MaxAllowedSize
		}
		if candidate <= 0 {
			candidate = meta.DefaultLength
		}
		if candidate > 0 {
			return candidate, "upstream", append([]int64(nil), meta.SupportedLengths...)
		}
		return 0, "none", append([]int64(nil), meta.SupportedLengths...)
	}
	if fallback > 0 {
		return fallback, "modelsdev", nil
	}
	return 0, "none", nil
}

func applyModelContextToInfo(info *pluginapi.ModelInfo, id string) {
	if info == nil {
		return
	}
	length, _, _ := effectiveModelContext(id, info.ContextLength)
	if length > 0 {
		info.ContextLength = length
	}
}

// reapplyModelContextSnapshotLength recomputes one snapshot entry's context
// length from the current override plus the recorded baseline.
//
// Snapshots bake the effective length in at build time. Selecting or clearing a
// tier therefore has to be applied to already-built snapshots, otherwise CPA and
// every client that reads the model registry keep seeing the previous value
// until the next upstream refresh. Recomputing in place is deliberate: it keeps
// the snapshot's ready state, so changing a tier never causes the transient
// "model catalog is not ready" outage that full invalidation would.
func (r *modelRuntime) reapplyModelContextSnapshotLength(authID, model string) bool {
	key := strings.ToLower(strings.TrimSpace(model))
	if key == "" {
		return false
	}
	base, hasBase := baseContextLengthForModel(model)
	override, hasOverride := contextOverrideForModel(model)
	if !hasBase && !hasOverride {
		return false
	}

	slot := r.authSlot(authID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	current := slot.current.Load()
	if current == nil {
		return false
	}
	next := cloneModelReadinessSnapshot(*current)
	changed := false
	for i := range next.Models {
		if strings.ToLower(strings.TrimSpace(next.Models[i].ID)) != key {
			continue
		}
		target := next.Models[i].ContextLength
		if hasOverride && override > 0 {
			target = override
		} else if hasBase {
			target = base
		}
		if target > 0 && next.Models[i].ContextLength != target {
			next.Models[i].ContextLength = target
			changed = true
		}
	}
	if !changed {
		return false
	}
	storeModelReadinessSnapshot(slot, next)
	return true
}

// reapplyModelContextSnapshots applies the current override state for one model
// across every cached snapshot.
func (r *modelRuntime) reapplyModelContextSnapshots(model string) bool {
	changed := false
	for _, file := range panelModelAuthFiles() {
		if r.reapplyModelContextSnapshotLength(file.ID, model) {
			changed = true
		}
	}
	return changed
}

// resetModelContextSnapshots removes an override from every cached snapshot.
func (r *modelRuntime) resetModelContextSnapshots(model string) bool {
	return r.reapplyModelContextSnapshots(model)
}
