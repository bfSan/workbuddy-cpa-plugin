package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxDiscoveredModelIDBytes = 512
const modelSourceRequestTimeout = 15 * time.Second

const maxModelCreditsBytes = 64

// Reasoning effort values are short upstream tokens ("low", "xhigh", "max").
// Bounded like the credits field: the values are display-only panel metadata,
// so a hostile catalog must not be able to bloat the cache with them.
const (
	maxModelEffortBytes = 32
	maxModelEfforts     = 16
)

type modelFacts struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Credits is the upstream per-model multiplier as reported verbatim, e.g.
	// "x0.29", "x2.20 credits", "x0.00". It is display/estimation metadata:
	// pluginapi.ModelInfo has no cost field, so it never reaches CPA billing.
	Credits                   string   `json:"credits,omitempty"`
	ContextLength             *int64   `json:"context_length,omitempty"`
	MaxCompletionTokens       *int64   `json:"max_completion_tokens,omitempty"`
	DefaultContextLength      *int64   `json:"default_context_length,omitempty"`
	SupportedContextLengths   []int64  `json:"supported_context_lengths,omitempty"`
	MaxAllowedSize            *int64   `json:"max_allowed_size,omitempty"`
	MaxInputTokens            *int64   `json:"max_input_tokens,omitempty"`
	SupportedInputModalities  []string `json:"supported_input_modalities,omitempty"`
	SupportedOutputModalities []string `json:"supported_output_modalities,omitempty"`
	// Thinking facts mirror the upstream reasoning object. They stay
	// plugin-side: pluginapi.ModelInfo describes the advertised model but has
	// no reasoning field, and the panel is what renders them.
	//
	// SupportedEfforts is the selectable effort list; DefaultEffort is the
	// tier upstream currently uses. A reasoning object that carries only
	// "effort" (the preset models) has that single value recorded in both, so
	// the panel still reports a level instead of "未上报".
	//
	// CanDisableThinking and SupportsReasoning are pointers on purpose: the
	// panel must tell "upstream did not say" from an explicit false, and a
	// bool would collapse those two into the same zero value.
	SupportedEfforts   []string `json:"supported_efforts,omitempty"`
	DefaultEffort      string   `json:"default_effort,omitempty"`
	CanDisableThinking *bool    `json:"can_disable_thinking,omitempty"`
	SupportsReasoning  *bool    `json:"supports_reasoning,omitempty"`
}

// cloneModelThinkingFacts deep-copies the reasoning facts. The levels slice must
// never alias its source, and the two tri-state bools must stay independent
// pointers so a copy can be compared and mutated safely.
func cloneModelThinkingFacts(facts modelFacts) modelFacts {
	facts.SupportedEfforts = append([]string(nil), facts.SupportedEfforts...)
	facts.CanDisableThinking = cloneBool(facts.CanDisableThinking)
	facts.SupportsReasoning = cloneBool(facts.SupportsReasoning)
	return facts
}

// fillMissingModelThinkingFacts copies reasoning facts only where the
// destination reported nothing. A destination that reported an empty level list
// is treated as "no information", matching fillMissingModelFacts' handling of
// the other slices.
func fillMissingModelThinkingFacts(dst *modelFacts, src modelFacts) {
	if len(dst.SupportedEfforts) == 0 {
		dst.SupportedEfforts = append([]string(nil), src.SupportedEfforts...)
	}
	if dst.DefaultEffort == "" {
		dst.DefaultEffort = src.DefaultEffort
	}
	if dst.CanDisableThinking == nil {
		dst.CanDisableThinking = cloneBool(src.CanDisableThinking)
	}
	if dst.SupportsReasoning == nil {
		dst.SupportsReasoning = cloneBool(src.SupportsReasoning)
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

type modelHTTPDo func(*http.Request, string) (*hostHTTPResponse, error)

type modelSourceFailureKind string

const (
	modelSourceTransportFailure modelSourceFailureKind = "transport"
	modelSourceHTTPFailure      modelSourceFailureKind = "http"
	modelSourceSchemaFailure    modelSourceFailureKind = "schema"
)

type modelSourceError struct {
	Kind       modelSourceFailureKind
	StatusCode int
	err        error
}

func (e *modelSourceError) Error() string {
	switch e.Kind {
	case modelSourceTransportFailure:
		return "model source transport failure"
	case modelSourceHTTPFailure:
		return fmt.Sprintf("model source HTTP %d", e.StatusCode)
	default:
		return "model source schema failure"
	}
}

func (e *modelSourceError) Unwrap() error {
	return e.err
}

type workBuddyRealm string

const (
	workBuddyRealmCN     workBuddyRealm = "cn"
	workBuddyRealmGlobal workBuddyRealm = "global"
)

type workBuddyEndpointKind string

const (
	workBuddyEndpointV3Config             workBuddyEndpointKind = "v3_config"
	workBuddyEndpointLegacyPersonalModels workBuddyEndpointKind = "legacy_personal_models"
)

type workBuddyCatalog struct {
	Realm    workBuddyRealm        `json:"realm"`
	Endpoint workBuddyEndpointKind `json:"endpoint"`
	Models   []modelFacts          `json:"models"`
}

type workBuddyAgentWire struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// workBuddyRichModelWire is one entry of /v3/config's data.models rich
// catalog. Only the fields the plugin can act on are decoded; unknown keys
// (promotions, tiers, related models) stay ignored on purpose.
type workBuddyContextWindowWire struct {
	DefaultLength    *int64  `json:"defaultLength"`
	SupportedLengths []int64 `json:"supportedLengths"`
}

// UnmarshalJSON accepts both the current object form and the older numeric
// contextWindow form still returned by some legacy fixtures/accounts.
func (w *workBuddyContextWindowWire) UnmarshalJSON(raw []byte) error {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		*w = workBuddyContextWindowWire{}
		return nil
	}
	if strings.HasPrefix(value, "{") {
		type alias workBuddyContextWindowWire
		var decoded alias
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		*w = workBuddyContextWindowWire(decoded)
		return nil
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	w.DefaultLength = &n
	w.SupportedLengths = []int64{n}
	return nil
}

type workBuddyRichModelWire struct {
	ID                string                      `json:"id"`
	Name              string                      `json:"name"`
	DescriptionEn     string                      `json:"descriptionEn"`
	DescriptionZh     string                      `json:"descriptionZh"`
	Credits           string                      `json:"credits"`
	Disabled          bool                        `json:"disabled"`
	ContextWindow     *workBuddyContextWindowWire `json:"contextWindow"`
	MaxAllowedSize    *int64                      `json:"maxAllowedSize"`
	MaxInputTokens    *int64                      `json:"maxInputTokens"`
	MaxTokens         *int64                      `json:"maxOutputTokens"`
	Reasoning         *workBuddyReasoningWire     `json:"reasoning"`
	SupportsReasoning *bool                       `json:"supportsReasoning"`
}

// workBuddyReasoningWire is the upstream per-model reasoning object. Upstream
// serves several shapes, all of which are just optional keys:
//
//	null
//	{"effort":"medium","summary":"auto"}                     — preset models
//	{"defaultEffort":"high","supportedEfforts":[...]}
//	{"canDisableThinking":true,"defaultEffort":"high",...}
//
// Every member is optional and decoded leniently: one wrong-typed member must
// not discard the effort information the rest of the object carries, and it
// must never fail the whole catalog.
type workBuddyReasoningWire struct {
	Effort             string   `json:"effort"`
	Summary            string   `json:"summary"`
	DefaultEffort      string   `json:"defaultEffort"`
	SupportedEfforts   []string `json:"supportedEfforts"`
	CanDisableThinking *bool    `json:"canDisableThinking"`
}

// UnmarshalJSON accepts the object form and degrades every other JSON value to
// "not reported". A reasoning shape the plugin does not understand is unknown
// capability, never a reason to drop an otherwise usable catalog.
func (w *workBuddyReasoningWire) UnmarshalJSON(raw []byte) error {
	*w = workBuddyReasoningWire{}
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" || !strings.HasPrefix(value, "{") {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	w.Effort = lenientWireString(fields["effort"])
	w.Summary = lenientWireString(fields["summary"])
	w.DefaultEffort = lenientWireString(fields["defaultEffort"])
	w.SupportedEfforts = lenientWireStringList(fields["supportedEfforts"])
	w.CanDisableThinking = lenientWireBool(fields["canDisableThinking"])
	return nil
}

// lenientWireString returns the decoded string, or "" for absent, null or any
// non-string JSON value.
func lenientWireString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// lenientWireStringList accepts a JSON array of strings. A bare string is also
// accepted and treated as a single-element list: upstream has shipped both
// shapes for other list-typed members, and one effort must not be lost.
// Non-string elements are skipped rather than aborting the whole list.
func lenientWireStringList(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return nil
		}
		out := make([]string, 0, len(values))
		for _, item := range values {
			if value := lenientWireString(item); strings.TrimSpace(value) != "" {
				out = append(out, value)
			}
		}
		return out
	}
	if value := lenientWireString(raw); strings.TrimSpace(value) != "" {
		return []string{value}
	}
	return nil
}

// lenientWireBool reports the decoded bool, or nil when upstream did not send a
// JSON boolean at all. nil is deliberately distinct from false: the panel must
// not present "upstream did not say" as "thinking cannot be disabled".
func lenientWireBool(raw json.RawMessage) *bool {
	var value bool
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &value
}

// workBuddyLegacyModelWire is the pre-/v3 config shape. The legacy catalog does
// not advertise reasoning today, so the two thinking members stay nil and the
// panel reports 未上报 rather than guessing. They are decoded anyway because
// upstream has been growing this object: if it ever ships them, the facts must
// carry them without a code change.
type workBuddyLegacyModelWire struct {
	ID                string                  `json:"id"`
	Name              string                  `json:"name"`
	Description       string                  `json:"description"`
	Credits           string                  `json:"credits"`
	Disabled          bool                    `json:"disabled"`
	ContextWindow     *int64                  `json:"contextWindow"`
	MaxTokens         *int64                  `json:"maxTokens"`
	Reasoning         *workBuddyReasoningWire `json:"reasoning"`
	SupportsReasoning *bool                   `json:"supportsReasoning"`
}

func parseWorkBuddyV3Config(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Agents []workBuddyAgentWire     `json:"agents"`
			Models []workBuddyRichModelWire `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode v3 config: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("v3 config business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("v3 config data is missing")
	}

	var modelIDs []string
	foundCLI := false
	for _, agent := range response.Data.Agents {
		if agent.Name != "cli" {
			continue
		}
		if foundCLI {
			return nil, fmt.Errorf("v3 config has multiple cli agents")
		}
		foundCLI = true
		modelIDs = agent.Models
	}
	if !foundCLI {
		return nil, fmt.Errorf("v3 config cli agent is missing")
	}

	// Every entitlement ID must still be non-empty: a blank one is a schema
	// problem, and skipping it would silently serve a short catalog.
	for _, rawID := range modelIDs {
		if strings.TrimSpace(rawID) == "" {
			return nil, fmt.Errorf("v3 config cli agent model ID is empty")
		}
	}

	// The catalog is the union of entitlement and rich entries, not just the
	// former. The cli agent's list is narrower than what the account can
	// actually call — several chat models appear only in the rich catalog — and
	// the rich list is also where the per-model metadata lives. Entitlement
	// order leads, then remaining rich entries that can serve chat.
	entitled := make(map[string]struct{}, len(modelIDs))
	models := make([]modelFacts, 0, len(modelIDs)+len(response.Data.Models))
	for _, rawID := range modelIDs {
		id := strings.TrimSpace(rawID)
		// A duplicate entitlement ID is still a schema problem: validateModelFacts
		// would reject it, and serving a quietly de-duplicated list hides that.
		if _, dup := entitled[id]; dup {
			return nil, fmt.Errorf("v3 config cli agent model ID is duplicated")
		}
		entitled[id] = struct{}{}
		models = append(models, modelFacts{ID: id})
	}
	for _, entry := range response.Data.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" || entry.Disabled {
			continue
		}
		if _, seen := entitled[id]; seen {
			continue
		}
		entitled[id] = struct{}{}
		if !modelKindChatUsable(classifyModelID(id)) {
			continue
		}
		models = append(models, modelFacts{ID: id})
	}

	// Rich metadata applies by ID to whatever survived.
	rich := make(map[string]workBuddyRichModelWire, len(response.Data.Models))
	for _, entry := range response.Data.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		rich[id] = entry
	}
	for i := range models {
		entry, ok := rich[models[i].ID]
		if !ok {
			continue
		}
		models[i].Name = strings.TrimSpace(entry.Name)
		models[i].Description = firstNonEmptyModelDescription(entry.DescriptionEn, entry.DescriptionZh)
		models[i].Credits = normalizeModelCredits(entry.Credits)
		if entry.ContextWindow != nil {
			models[i].DefaultContextLength = entry.ContextWindow.DefaultLength
			models[i].SupportedContextLengths = append([]int64(nil), entry.ContextWindow.SupportedLengths...)
		}
		models[i].MaxAllowedSize = entry.MaxAllowedSize
		models[i].MaxInputTokens = entry.MaxInputTokens
		models[i].ContextLength = entry.MaxInputTokens
		if models[i].ContextLength == nil && entry.ContextWindow != nil {
			models[i].ContextLength = entry.ContextWindow.DefaultLength
		}
		models[i].MaxCompletionTokens = entry.MaxTokens
		applyWorkBuddyReasoningWire(&models[i], entry.Reasoning)
		models[i].SupportsReasoning = cloneBool(entry.SupportsReasoning)
	}
	return validateModelFacts(models)
}

// applyWorkBuddyReasoningWire copies the upstream reasoning object onto one set
// of facts. An object that reports only "effort" (the preset models) has no
// selectable list upstream, so that single effort becomes both the level list
// and the default: the panel then reports the real level instead of "未上报"
// without inventing alternatives the provider never offered.
func applyWorkBuddyReasoningWire(facts *modelFacts, reasoning *workBuddyReasoningWire) {
	if facts == nil || reasoning == nil {
		return
	}
	efforts := normalizeModelEfforts(reasoning.SupportedEfforts)
	defaultEffort := normalizeModelEffort(reasoning.DefaultEffort)
	if len(efforts) == 0 {
		if fixed := normalizeModelEffort(reasoning.Effort); fixed != "" {
			efforts = []string{fixed}
			if defaultEffort == "" {
				defaultEffort = fixed
			}
		}
	}
	facts.SupportedEfforts = efforts
	facts.DefaultEffort = defaultEffort
	facts.CanDisableThinking = cloneBool(reasoning.CanDisableThinking)
}

func firstNonEmptyModelDescription(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// mergeModelCatalog unions two catalogs of the same account. The primary list
// leads: its order and metadata win for an ID both carry, because it is the
// view the desktop client itself sees. Entries that only the secondary list
// has are appended with their own metadata, so neither identity's slice of the
// catalogue is lost.
func mergeModelCatalog(primary, secondary []modelFacts) []modelFacts {
	if len(secondary) == 0 {
		return primary
	}
	out := make([]modelFacts, 0, len(primary)+len(secondary))
	seen := make(map[string]int, len(primary)+len(secondary))
	for _, model := range primary {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		seen[id] = len(out)
		out = append(out, model)
	}
	for _, model := range secondary {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if index, exists := seen[id]; exists {
			// The leading catalog already describes this model; only fill gaps
			// it left blank rather than overwriting what it reported.
			fillMissingModelFacts(&out[index], model)
			continue
		}
		seen[id] = len(out)
		out = append(out, model)
	}
	return out
}

// normalizeModelCredits keeps the upstream multiplier verbatim but bounded:
// the field is display-only, so a hostile value must not bloat the catalog or
// the cache. Upstream formats vary ("x0.29", "x2.20 credits", "x0.00").
func normalizeModelCredits(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) > maxModelCreditsBytes {
		return ""
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
	}) >= 0 {
		return ""
	}
	return value
}

// normalizeModelEffort bounds one effort token. Like the credits field these
// values are display-only panel metadata, so an oversized or multi-line value
// upstream must be dropped rather than stored.
func normalizeModelEffort(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxModelEffortBytes {
		return ""
	}
	if strings.IndexFunc(value, func(r rune) bool {
		return r == '\r' || r == '\n' || r == 0x85 || r == 0x2028 || r == 0x2029
	}) >= 0 {
		return ""
	}
	return value
}

// normalizeModelEfforts cleans a selectable effort list: it drops blank and
// invalid entries, de-duplicates while keeping upstream order (the panel shows
// the list verbatim), and caps the total so a hostile catalog cannot bloat the
// cache. It never fails the caller: a bad level degrades that level only.
func normalizeModelEfforts(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		value := normalizeModelEffort(raw)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) == maxModelEfforts {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func parseWorkBuddyLegacyModels(raw []byte) ([]modelFacts, error) {
	var response struct {
		Code *int `json:"code"`
		Data *struct {
			Models []workBuddyLegacyModelWire `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("decode legacy models: %w", err)
	}
	if response.Code == nil || *response.Code != 0 {
		return nil, fmt.Errorf("legacy models business code is not successful")
	}
	if response.Data == nil {
		return nil, fmt.Errorf("legacy models data is missing")
	}

	models := make([]modelFacts, len(response.Data.Models))
	for i, model := range response.Data.Models {
		models[i] = modelFacts{
			ID:                  model.ID,
			Name:                model.Name,
			Description:         firstNonEmptyModelDescription(model.Description),
			Credits:             normalizeModelCredits(model.Credits),
			ContextLength:       model.ContextWindow,
			MaxInputTokens:      model.ContextWindow,
			MaxCompletionTokens: model.MaxTokens,
		}
		// The legacy endpoint does not report reasoning today. Decode it anyway
		// so the facts stay empty (panel: 未上报) rather than wrong if upstream
		// ever starts sending it.
		applyWorkBuddyReasoningWire(&models[i], model.Reasoning)
		models[i].SupportsReasoning = cloneBool(model.SupportsReasoning)
	}
	models, err := validateModelFacts(models)
	if err != nil {
		return nil, err
	}
	enabled := make([]modelFacts, 0, len(models))
	for i, model := range models {
		if !response.Data.Models[i].Disabled {
			enabled = append(enabled, model)
		}
	}
	return validateModelFacts(enabled)
}

func validateModelFacts(models []modelFacts) ([]modelFacts, error) {
	if len(models) == 0 {
		return nil, fmt.Errorf("model snapshot is empty")
	}

	validated := make([]modelFacts, len(models))
	seen := make(map[string]struct{}, len(models))
	for i, model := range models {
		model.ID = strings.TrimSpace(model.ID)
		model.Name = strings.TrimSpace(model.Name)
		model.Credits = normalizeModelCredits(model.Credits)
		if model.ID == "" {
			return nil, fmt.Errorf("model ID is empty")
		}
		if len(model.ID) > maxDiscoveredModelIDBytes {
			return nil, fmt.Errorf("model ID exceeds %d bytes", maxDiscoveredModelIDBytes)
		}
		if _, exists := seen[model.ID]; exists {
			return nil, fmt.Errorf("model ID is duplicated")
		}
		if model.ContextLength != nil && *model.ContextLength < 0 {
			return nil, fmt.Errorf("model context length is negative")
		}
		if model.MaxCompletionTokens != nil && *model.MaxCompletionTokens < 0 {
			return nil, fmt.Errorf("model max completion tokens is negative")
		}
		if model.SupportedInputModalities != nil {
			model.SupportedInputModalities = append([]string{}, model.SupportedInputModalities...)
		}
		if model.SupportedOutputModalities != nil {
			model.SupportedOutputModalities = append([]string{}, model.SupportedOutputModalities...)
		}
		// Reasoning facts are re-normalized here because validateModelFacts is
		// also the gate for catalogs read back from cache and from the models.dev
		// overlay, not only for freshly fetched ones.
		model.SupportedEfforts = normalizeModelEfforts(model.SupportedEfforts)
		model.DefaultEffort = normalizeModelEffort(model.DefaultEffort)
		model.CanDisableThinking = cloneBool(model.CanDisableThinking)
		model.SupportsReasoning = cloneBool(model.SupportsReasoning)
		// The provider's default effort is data, not policy: exactly like
		// defaultLength above, a default that is not in the advertised list must
		// not fail the whole catalog. Unlike defaultLength the value is NOT
		// injected into the list — the panel renders the list verbatim as the
		// selectable tiers, and adding an unlisted level there would advertise a
		// tier the provider never offered. Only the default is kept, so the panel
		// can highlight nothing rather than highlight a level that is not listed.
		seen[model.ID] = struct{}{}
		validated[i] = model
	}
	return validated, nil
}

func fetchWorkBuddyCatalog(sa *storedAuth, callbackID string, do modelHTTPDo) (workBuddyCatalog, error) {
	if sa == nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: fmt.Errorf("stored auth is nil")}
	}
	realm, err := workBuddyRealmFromAccessToken(sa.Auth.AccessToken)
	if err != nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}

	base := upstreamBaseCN
	origin := originReferer
	if realm == workBuddyRealmGlobal {
		base = upstreamBaseGlobal
		origin = originRefererGlobal
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelSourceRequestTimeout)
	defer cancel()

	request := func(path, userAgent string) (*hostHTTPResponse, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		backendHeaders(req, sa)
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		resp, err := do(req, callbackID)
		if err != nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: err}
		}
		if resp == nil {
			return nil, &modelSourceError{Kind: modelSourceTransportFailure, err: fmt.Errorf("empty HTTP response")}
		}
		return resp, nil
	}

	resp, err := request("/v3/config", workBuddyDesktopUA)
	if err != nil {
		return workBuddyCatalog{}, err
	}
	if resp.StatusCode == http.StatusOK {
		models, err := parseWorkBuddyV3Config(resp.Body)
		if err != nil {
			return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
		}
		// Upstream serves a different slice of the catalog per client identity:
		// the desktop list carries the fast/balanced/deep presets and the
		// desktop default, while the CLI list carries the default model and a
		// few entries the desktop list omits. Neither is a superset, so the
		// served catalog is their union. The CLI fetch is best-effort: a failure
		// there must not discard the desktop catalog that already parsed.
		if cliResp, cliErr := request("/v3/config", clientUA); cliErr == nil && cliResp.StatusCode == http.StatusOK {
			if cliModels, parseErr := parseWorkBuddyV3Config(cliResp.Body); parseErr == nil {
				models = mergeModelCatalog(models, cliModels)
			}
		}
		return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointV3Config, Models: models}, nil
	}
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
	}

	resp, err = request("/console/enterprises/personal/models", clientUA)
	if err != nil {
		return workBuddyCatalog{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceHTTPFailure, StatusCode: resp.StatusCode}
	}
	models, err := parseWorkBuddyLegacyModels(resp.Body)
	if err != nil {
		return workBuddyCatalog{}, &modelSourceError{Kind: modelSourceSchemaFailure, err: err}
	}
	return workBuddyCatalog{Realm: realm, Endpoint: workBuddyEndpointLegacyPersonalModels, Models: models}, nil
}

// workBuddyRealmFromAccessToken decodes unverified JWT routing facts only.
func workBuddyRealmFromAccessToken(accessToken string) (workBuddyRealm, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT payload: %w", err)
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("decode JWT claims: %w", err)
	}
	issuer, err := url.Parse(claims.Issuer)
	if err != nil || !issuer.IsAbs() || issuer.Hostname() == "" {
		return "", fmt.Errorf("JWT issuer is not an absolute URL")
	}
	switch strings.ToLower(issuer.Hostname()) {
	case "codebuddy.cn", "www.codebuddy.cn", "copilot.tencent.com":
		return workBuddyRealmCN, nil
	case "workbuddy.ai", "www.workbuddy.ai":
		return workBuddyRealmGlobal, nil
	default:
		return "", fmt.Errorf("JWT issuer host is unsupported")
	}
}
