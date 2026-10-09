// models.go implements the ModelProvider capability: static/per-auth model
// lists, alias reverse resolution (client-facing alias -> upstream model id),
// the plugin-owned hidden-model filter, and the host-config
// oauth-excluded-models filter.
package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func defaultModelInfo(id, name string) pluginapi.ModelInfo {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if name == "" {
		name = id
	}
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
}

func wbModels() []pluginapi.ModelInfo {
	return []pluginapi.ModelInfo{defaultModelInfo("auto", "")}
}

func cacheModelAliases(host pluginapi.HostConfigSummary) {
	entries := host.OAuthModelAlias[providerName]
	if len(entries) == 0 {
		// Host may key the channel case-insensitively; fall back to a scan.
		for channel, list := range host.OAuthModelAlias {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				entries = list
				break
			}
		}
	}
	byAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		alias := strings.TrimSpace(e.Alias)
		if name == "" || alias == "" || strings.EqualFold(name, alias) {
			continue
		}
		byAlias[strings.ToLower(alias)] = name
	}
	modelAliasCache.Lock()
	modelAliasCache.byAlias = byAlias
	modelAliasCache.Unlock()
}

// resolveUpstreamModel maps an aliased requested model back to the real
// upstream model ID. Returns the input unchanged when nothing matches.
func resolveUpstreamModel(model string, attributes map[string]string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	key := strings.ToLower(m)
	if name, ok := parseModelAliasAttribute(attributes)[key]; ok {
		return name
	}
	modelAliasCache.RLock()
	name, ok := modelAliasCache.byAlias[key]
	modelAliasCache.RUnlock()
	if ok {
		return name
	}
	// A preset alias carries its own upstream credits and capabilities, so it is
	// forwarded as itself rather than rewritten to some concrete model behind
	// it. Guessing a replacement would silently change both the response and
	// the billed multiplier.
	return m
}

// parseModelAliasAttribute decodes a per-auth alias override from auth
// attributes. Accepts JSON ([{"name":...,"alias":...}] or {alias:name}) or
// comma-separated "alias=name" pairs.
func parseModelAliasAttribute(attributes map[string]string) map[string]string {
	if len(attributes) == 0 {
		return nil
	}
	raw := ""
	for _, k := range []string{"model_alias", "model-alias", "oauth-model-alias"} {
		if v := strings.TrimSpace(attributes[k]); v != "" {
			raw = v
			break
		}
	}
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	add := func(name, alias string) {
		name, alias = strings.TrimSpace(name), strings.TrimSpace(alias)
		if name != "" && alias != "" && !strings.EqualFold(name, alias) {
			out[strings.ToLower(alias)] = name
		}
	}
	if strings.HasPrefix(raw, "[") {
		var list []struct {
			Name  string `json:"name"`
			Alias string `json:"alias"`
		}
		if json.Unmarshal([]byte(raw), &list) == nil {
			for _, e := range list {
				add(e.Name, e.Alias)
			}
			return out
		}
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if json.Unmarshal([]byte(raw), &m) == nil {
			for alias, name := range m {
				add(name, alias)
			}
			return out
		}
	}
	for _, pair := range strings.Split(raw, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			add(kv[1], kv[0])
		}
	}
	return out
}

// filterExcludedModels removes models listed in oauth-excluded-models for
// the workbuddy provider. The host passes this config via HostConfigSummary.
func filterExcludedModels(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	// Try exact provider match, then case-insensitive scan.
	excluded := host.ExcludedModels[providerName]
	if len(excluded) == 0 {
		for channel, list := range host.ExcludedModels {
			if strings.EqualFold(strings.TrimSpace(channel), providerName) {
				excluded = list
				break
			}
		}
	}
	if len(excluded) == 0 {
		return models
	}
	excludeSet := make(map[string]struct{}, len(excluded))
	for _, m := range excluded {
		excludeSet[strings.ToLower(strings.TrimSpace(m))] = struct{}{}
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, m := range models {
		if _, skip := excludeSet[strings.ToLower(m.ID)]; skip {
			continue
		}
		out = append(out, m)
	}
	return out
}

// publishUsage reports one upstream attempt into CPAMP request monitoring.
// requestedModel is client-facing (may be alias); upstreamModel is resolved.

func handleModelStatic(raw []byte) ([]byte, error) {
	var req pluginapi.StaticModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	models := wbModels()
	models = filterHiddenModels(models)
	models = filterExcludedModels(models, req.Host)
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

func handleModelForAuth(raw []byte) ([]byte, error) {
	var req authModelRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cacheModelAliases(req.Host)
	snapshot := currentModelRuntime().ensureForAuth(req)
	models := []pluginapi.ModelInfo{}
	if snapshot.State.executable() {
		// The snapshot intentionally stores the complete upstream catalog so
		// hidden entries remain restorable. Apply the operator overlay only to
		// this response copy, after cloning shared state.
		models = applyModelOverlay(cloneModelInfos(snapshot.Models), loadedModelOverlayForRead())
		models = filterHiddenModels(models)
		models = filterExcludedModels(models, req.Host)
		models = filterCoolingModels(req, models)
	}
	return okEnvelope(pluginapi.ModelResponse{Provider: providerName, Models: models})
}

// filterCoolingModels removes the models this account is currently cooling.
//
// Why this exists: the plugin recorded per-(account, model) throttling for the
// panel, but model.for_auth ignored it, so CPA kept believing the pair was
// healthy. With routing.session-affinity enabled a session stays pinned to one
// account for its whole TTL, so an operator saw a session keep hammering a
// throttled pair instead of failing over to another account.
//
// Withholding the model is what makes CPA route around the bad pair: CPA
// registers exactly this response per auth (RegisterClient, with the auth ID as
// client ID), and its selection loop skips any account whose registry does not
// carry the requested model (see authSupportsRouteModel in
// conductor_selection.go).
//
// Only the (account, model) pair is withheld, never the whole account: cooldown
// state is per model by design (see cooldown.go), and one degraded model must
// not black out the rest of that account's catalog.
//
// The filter affects only this response. The snapshot and the panel's admin
// catalog keep the full list, because an operator has to see a cooling model in
// order to clear it.
func filterCoolingModels(req authModelRequestWire, models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	if len(models) == 0 {
		return models
	}
	authID := strings.TrimSpace(req.AuthID)
	if authID == "" {
		// Nothing scopes the cooldown without an auth ID, and matching against
		// every account's state could hide a perfectly healthy model.
		return models
	}
	cooling := coolingModelSetFor(authID)
	if len(cooling) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, skip := cooling[id]; skip {
			continue
		}
		// Cooldowns are keyed by the upstream model name (recordUpstreamFailure
		// receives resolveUpstreamModel's output) while this response carries
		// catalog IDs, and an alias can sit between the two. Try the resolved
		// name as well so an aliased catalog ID still matches. When neither
		// matches, the model is kept: a missed filter only preserves today's
		// behaviour, whereas a false positive would make a working model vanish.
		if resolved := resolveUpstreamModel(id, req.Attributes); resolved != id {
			if _, skip := cooling[resolved]; skip {
				continue
			}
		}
		out = append(out, model)
	}
	return out
}

// filterHiddenModels applies the persistent plugin-owned hidden_models list.
// CPA does not have a plugin-triggered "refresh registry" RPC, so this filter
// must be applied to model.static/model.for_auth responses; a plugin config
// reload makes CPA re-register those responses.
func filterHiddenModels(models []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	hidden := currentHiddenModels()
	if len(hidden) == 0 || len(models) == 0 {
		return models
	}
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		if _, skip := hidden[strings.TrimSpace(model.ID)]; skip {
			continue
		}
		out = append(out, model)
	}
	return out
}

func currentHiddenModels() map[string]struct{} {
	cfg := currentFeatureRuntime()
	if cfg == nil || len(cfg.hiddenModels) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(cfg.hiddenModels))
	for _, id := range cfg.hiddenModels {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}
