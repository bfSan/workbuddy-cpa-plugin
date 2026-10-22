package main

import (
	"sort"
	"strings"
	"time"
)

// panelRealmSnapshotSource is one account's published snapshot for one realm,
// tagged with the auth ID that produced it. The auth ID is what makes the
// per-model tie-break deterministic.
type panelRealmSnapshotSource struct {
	authID   string
	snapshot modelReadinessSnapshot
}

// panelRealmSelection is the union of every usable account snapshot in a realm,
// plus the latest usable snapshot kept for display.
//
// It used to hold exactly one snapshot. Upstream trims the catalog per account,
// so two CN accounts can each advertise models the other never receives
// (observed: 50 vs 52 IDs, 48 shared). Picking one snapshot dropped both
// accounts' exclusives, and the panel rendered "未加载" for a model that realm
// actually serves. The union matches baseModelCatalogTyped, which feeds the
// `models` field from every account, so the two views finally agree.
type panelRealmSelection struct {
	// facts is the union of every usable snapshot's ModelFacts, keyed by
	// trimmed model ID. A duplicate ID always resolves to the same winner for a
	// given set of snapshots (see panelRealmFactBetter), so panel refreshes
	// cannot make credits or context jump.
	facts map[string]modelFacts
	// order keeps first-seen insertion order so the emitted rows stay stable
	// without depending on map iteration.
	order []string
	// factSource records which snapshot each merged fact came from, so a later
	// duplicate can be compared against the current winner on (fetchedAt, authID)
	// without re-deriving provenance.
	factSource map[string]modelFactsSource
	// promotions is the union of every usable snapshot's campaigns in this
	// realm, keyed by campaign ID. Like facts, a duplicate ID resolves to a
	// deterministic winner so the panel cannot show a campaign that flickers
	// between two accounts' copies on consecutive refreshes.
	promotions map[string]modelPromotion
	// promotionOrder keeps first-seen insertion order for stable emission.
	promotionOrder []string
	// promotionSource records provenance for the duplicate tie-break.
	promotionSource map[string]modelFactsSource
	// tiers is the union of the realm's tier rows, keyed by tier ID.
	tiers map[string]modelTier
	// display is the newest usable snapshot, kept only for realm_status's
	// state/source/fetched_at. loaded is not derived from it: one stale account
	// must not make the whole realm look unloaded while its rows show facts.
	display modelReadinessSnapshot
	// displayAuthID is the auth ID behind display, used to break ties in the
	// same deterministic order as the fact merge.
	displayAuthID string
	// hasDisplay is false when no account reported a usable snapshot, which is
	// the only case that legitimately renders "未加载".
	hasDisplay bool
	// loaded is true when at least one account in the realm has a usable
	// snapshot, i.e. the realm has facts to show. buildPanelRealmModels and
	// buildPanelRealmStatus both read this same value, so status can never claim
	// a loaded realm whose rows are all not_loaded.
	loaded bool
}

func panelRealmSelections() map[workBuddyRealm]panelRealmSelection {
	selected := make(map[workBuddyRealm]panelRealmSelection)
	runtime := activeModelRuntime.Load()
	if runtime == nil {
		return selected
	}
	files, err := panelHostAuthList()
	if err != nil {
		return selected
	}
	for _, file := range files {
		snap := runtime.snapshotForAuthID(file.ID)
		// Realm and facts are snapshot-owned metadata. Do not inspect auth
		// credentials or infer a realm from an account that has not reported it.
		if snap.Realm != workBuddyRealmCN && snap.Realm != workBuddyRealmGlobal {
			continue
		}
		// Failed/loading snapshots contribute no rows but are still tracked so a
		// realm whose every account failed reports failed rather than not_loaded.
		// Only an executable snapshot with facts makes the realm loaded.
		selection := selected[snap.Realm]
		if snap.executable() && len(snap.ModelFacts) > 0 {
			selection.loaded = true
			selection = panelRealmSelectionAddFacts(selection, snap, file.ID)
		}
		if !selection.hasDisplay || panelRealmSnapshotBetter(snap, selection.display) ||
			(panelRealmSnapshotEquivalent(snap, selection.display) &&
				(!selection.hasDisplay || file.ID < selection.displayAuthID)) {
			selection.display = snap
			selection.displayAuthID = file.ID
			selection.hasDisplay = true
		}
		selected[snap.Realm] = selection
	}
	return selected
}

// panelRealmSelectionAddFacts merges one account's facts into the realm union.
func panelRealmSelectionAddFacts(selection panelRealmSelection, snap modelReadinessSnapshot, authID string) panelRealmSelection {
	if selection.facts == nil {
		selection.facts = make(map[string]modelFacts)
	}
	if selection.factSource == nil {
		selection.factSource = make(map[string]modelFactsSource)
	}
	source := modelFactsSource{snapshot: snap, authID: authID}
	for _, fact := range snap.ModelFacts {
		id := strings.TrimSpace(fact.ID)
		if id == "" {
			continue
		}
		existing, seen := selection.facts[id]
		if !seen {
			selection.facts[id] = cloneModelFacts(fact)
			selection.factSource[id] = source
			selection.order = append(selection.order, id)
			continue
		}
		// A duplicate ID must resolve the same way on every refresh, so the
		// winner is chosen by a total order over (fetchedAt, authID) rather
		// than by whichever snapshot happened to be visited last.
		if panelRealmFactWins(fact, existing, source, selection.factSource[id]) {
			selection.facts[id] = cloneModelFacts(fact)
			selection.factSource[id] = source
		}
	}
	selection = panelRealmSelectionAddPromotions(selection, snap, source)
	return selection
}

// panelRealmSelectionAddPromotions merges one account's campaigns into the realm
// union.
//
// Campaigns are authored server-side and are identical across the accounts of a
// realm in practice, but they are still merged with the same deterministic
// tie-break as facts rather than "first one wins": the union must not depend on
// the order the host happens to list auth files in, or a panel refresh could
// swap which copy of a campaign is shown.
func panelRealmSelectionAddPromotions(selection panelRealmSelection, snap modelReadinessSnapshot, source modelFactsSource) panelRealmSelection {
	if len(snap.Promotions) == 0 && len(snap.Tiers) == 0 {
		return selection
	}
	if selection.promotions == nil {
		selection.promotions = make(map[string]modelPromotion)
	}
	if selection.promotionSource == nil {
		selection.promotionSource = make(map[string]modelFactsSource)
	}
	if selection.tiers == nil {
		selection.tiers = make(map[string]modelTier)
	}
	for _, promo := range snap.Promotions {
		id := strings.TrimSpace(promo.ID)
		if id == "" {
			continue
		}
		_, seen := selection.promotions[id]
		if !seen {
			selection.promotions[id] = clonePromotion(promo)
			selection.promotionOrder = append(selection.promotionOrder, id)
			selection.promotionSource[id] = source
			continue
		}
		if panelRealmSourceWins(source, selection.promotionSource[id]) {
			selection.promotions[id] = clonePromotion(promo)
			selection.promotionSource[id] = source
		}
	}
	for _, tier := range snap.Tiers {
		id := strings.TrimSpace(tier.ID)
		if id == "" {
			continue
		}
		if _, seen := selection.tiers[id]; !seen {
			selection.tiers[id] = tier
		}
	}
	return selection
}

// panelRealmSourceWins is the campaign analogue of panelRealmFactWins: newest
// fetch wins, ties broken by the lexicographically smallest auth ID.
func panelRealmSourceWins(candidate, current modelFactsSource) bool {
	if candidate.authID == current.authID {
		return false
	}
	if !candidate.snapshot.ModelsFetchedAt.Equal(current.snapshot.ModelsFetchedAt) {
		return candidate.snapshot.ModelsFetchedAt.After(current.snapshot.ModelsFetchedAt)
	}
	return candidate.authID < current.authID
}

// realmPromotions returns the realm's campaigns in first-seen order.
func (s panelRealmSelection) realmPromotions() []modelPromotion {
	if len(s.promotions) == 0 {
		return nil
	}
	out := make([]modelPromotion, 0, len(s.promotions))
	for _, id := range s.promotionOrder {
		if promo, ok := s.promotions[id]; ok {
			out = append(out, promo)
		}
	}
	return out
}

// realmTiers returns the realm's tier rows, ordered by ID for stability.
func (s panelRealmSelection) realmTiers() []modelTier {
	if len(s.tiers) == 0 {
		return nil
	}
	out := make([]modelTier, 0, len(s.tiers))
	for _, tier := range s.tiers {
		out = append(out, tier)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelFactsSource identifies which snapshot a merged fact came from, so a later
// duplicate can be compared against the winner without re-deriving provenance.
type modelFactsSource struct {
	snapshot modelReadinessSnapshot
	authID   string
}

// panelRealmFactWins reports whether candidate should replace current for the
// same model ID inside one realm. The order is "latest ModelsFetchedAt wins,
// ties broken by the lexicographically smallest auth ID", which is a total order
// over distinct (authID) sources and therefore independent of map iteration.
func panelRealmFactWins(candidate modelFacts, current modelFacts, candidateSource, currentSource modelFactsSource) bool {
	if candidateSource.authID == currentSource.authID {
		return false
	}
	if !candidateSource.snapshot.ModelsFetchedAt.Equal(currentSource.snapshot.ModelsFetchedAt) {
		return candidateSource.snapshot.ModelsFetchedAt.After(currentSource.snapshot.ModelsFetchedAt)
	}
	return candidateSource.authID < currentSource.authID
}

// panelRealmSnapshotBetter orders snapshots for display. An executable snapshot
// beats a failed one; among equals the newest ModelsFetchedAt wins.
func panelRealmSnapshotBetter(candidate, current modelReadinessSnapshot) bool {
	if candidate.executable() != current.executable() {
		return candidate.executable()
	}
	return candidate.ModelsFetchedAt.After(current.ModelsFetchedAt)
}

func panelRealmSnapshotEquivalent(a, b modelReadinessSnapshot) bool {
	return a.executable() == b.executable() && a.ModelsFetchedAt.Equal(b.ModelsFetchedAt)
}

// buildPanelRealmCatalog selects once for both response fields. It reads only
// auth IDs and published snapshots, never credentials or upstream endpoints.
func buildPanelRealmCatalog() ([]map[string]any, map[string]any) {
	selected := panelRealmSelections()
	return buildPanelRealmModels(selected), buildPanelRealmStatus(selected)
}

// buildPanelPromotionsQuery serves the activity panel independently from the
// model catalog. Promotions are per-realm (CN and Global receive different
// /v3/config payloads), so the response is bucketed by realm just like
// realm_models. The raw normalized windows are returned; the browser decides
// whether each one is active using the supplied UTC clock and can therefore
// flip at a daily boundary without another upstream request.
func buildPanelPromotionsQuery() map[string]any {
	selected := panelRealmSelections()
	promotions := make(map[string][]modelPromotion, 2)
	tiers := make(map[string][]modelTier, 2)
	status := make(map[string]any, 2)
	fetched := make(map[string]time.Time, 2)
	now := time.Now()
	for _, realm := range panelRealmList() {
		key := string(realm)
		selection := selected[realm]
		promotions[key] = selection.realmPromotions()
		tiers[key] = selection.realmTiers()
		clock := promotionClock(promotions[key], now)
		status[key] = map[string]any{
			"loaded":         selection.loaded,
			"status":         promotionPanelStatus(selection),
			"clock":          clock,
			"promotionCount": len(promotions[key]),
		}
		if selection.hasDisplay {
			fetched[key] = selection.display.ModelsFetchedAt
		}
	}
	return map[string]any{
		"promotions": promotions,
		"tiers":      tiers,
		"status":     status,
		"fetched_at": fetched,
		"clock":      promoClock{Unix: now.UTC().Unix(), Minute: now.UTC().Hour()*60 + now.UTC().Minute(), Zone: "UTC"},
	}
}

func promotionPanelStatus(selection panelRealmSelection) string {
	if selection.loaded && len(selection.promotions) > 0 {
		return "loaded"
	}
	if selection.hasDisplay && selection.display.State == modelFailed {
		return "failed"
	}
	if selection.loaded {
		return "empty"
	}
	return "not_loaded"
}

func buildPanelRealmModels(selected map[workBuddyRealm]panelRealmSelection) []map[string]any {
	byID := make(map[string]map[string]any)
	order := make([]string, 0)
	for _, realm := range panelRealmList() {
		selection := selected[realm]
		if !selection.loaded {
			// No usable snapshot in this realm: every row's cell must say
			// not_loaded, matching realm_status' loaded=false for the realm.
			continue
		}
		for _, id := range selection.order {
			fact := selection.facts[id]
			row := byID[id]
			if row == nil {
				row = map[string]any{"id": id, "name": fact.Name}
				byID[id] = row
				order = append(order, id)
			}
			if strings.TrimSpace(fact.Name) != "" {
				row["name"] = fact.Name
			}
			row[string(realm)] = realmFactPanelValue(fact)
		}
	}

	out := make([]map[string]any, 0, len(order))
	for _, id := range order {
		row := byID[id]
		for _, realm := range panelRealmList() {
			if _, ok := row[string(realm)]; ok {
				continue
			}
			// The realm has facts but not this ID: the model is genuinely absent
			// from the account catalog, which is a different claim than "we never
			// loaded anything". `absent` renders "—", `not_loaded` renders
			// "未加载".
			if selected[realm].loaded {
				row[string(realm)] = map[string]any{"status": "absent", "present": false}
			} else {
				row[string(realm)] = map[string]any{"status": "not_loaded", "present": nil}
			}
		}
		out = append(out, row)
	}
	sortPanelRealmRows(out)
	return out
}

// sortPanelRealmRows keeps the response order stable across refreshes. The
// per-realm order lists are built by iterating auth files, whose order the host
// does not promise, so the union must be sorted once instead of inheriting it.
func sortPanelRealmRows(rows []map[string]any) {
	sort.SliceStable(rows, func(i, j int) bool {
		left, _ := rows[i]["id"].(string)
		right, _ := rows[j]["id"].(string)
		return left < right
	})
}

func buildPanelRealmStatus(selected map[workBuddyRealm]panelRealmSelection) map[string]any {
	status := make(map[string]any, 2)
	for _, realm := range panelRealmList() {
		selection := selected[realm]
		if !selection.loaded {
			entry := map[string]any{"status": "not_loaded", "loaded": false}
			// A realm whose accounts failed reports the failure instead of
			// pretending nothing was ever attempted. loaded stays false and the
			// rows stay not_loaded, so status and rows still agree.
			if selection.hasDisplay {
				entry["status"] = string(selection.display.State)
				entry["source"] = string(selection.display.ModelSource)
				entry["fetched_at"] = selection.display.ModelsFetchedAt
			}
			status[string(realm)] = entry
			continue
		}
		entry := map[string]any{"loaded": true}
		if selection.hasDisplay {
			// state/source/fetched_at describe the newest usable snapshot; loaded
			// describes whether the realm has any usable snapshot at all.
			entry["status"] = string(selection.display.State)
			entry["source"] = string(selection.display.ModelSource)
			entry["fetched_at"] = selection.display.ModelsFetchedAt
		} else {
			entry["status"] = "loaded"
		}
		status[string(realm)] = entry
	}
	return status
}

// panelRealmList fixes the realm order so the response field order never depends
// on map iteration.
func panelRealmList() []workBuddyRealm {
	return []workBuddyRealm{workBuddyRealmCN, workBuddyRealmGlobal}
}

// realmFactThinkingValue renders the upstream reasoning capability for one
// realm cell. The status vocabulary is deliberately conservative:
//
//	supported    — upstream advertised at least one effort level
//	unsupported  — upstream explicitly said supportsReasoning is false
//	off_only     — upstream allows disabling thinking but named no level
//	               (forward-compatible slot; the observed catalog never
//	               reports this combination)
//	unknown      — upstream did not report reasoning, or reported it without
//	               any level and without an explicit supportsReasoning
//
// "unknown" is what an unreported capability must stay: inventing "unsupported"
// from a missing field would tell the operator a model cannot think when the
// upstream simply did not say. can_disabled is nil (JSON null) when upstream
// omitted canDisableThinking, distinct from an explicit false.
func realmFactThinkingValue(f modelFacts) map[string]any {
	levels := append([]string{}, f.SupportedEfforts...)
	status := "unknown"
	switch {
	case len(levels) > 0:
		status = "supported"
	case f.SupportsReasoning != nil && !*f.SupportsReasoning:
		status = "unsupported"
	case f.CanDisableThinking != nil && *f.CanDisableThinking:
		status = "off_only"
	}
	return map[string]any{
		"status":      status,
		"default":     f.DefaultEffort,
		"can_disable": cloneBool(f.CanDisableThinking),
		"levels":      levels,
	}
}

// realmFactEffectiveContextLength reports the context length the plugin
// currently serves for one model. The snapshot's ModelFacts hold the raw
// upstream catalog ("before models.dev enrichment and process-wide overrides"),
// so the served value has to be recomputed here for an operator's tier
// selection to show up.
//
// The nil-able shape is kept: a model whose length is unknown stays JSON null
// rather than becoming 0, which the panel would render as a real tier.
func realmFactEffectiveContextLength(f modelFacts) *int64 {
	if length, _, _ := effectiveModelContext(f.ID, derefInt64(f.ContextLength)); length > 0 {
		return &length
	}
	return cloneInt64(f.ContextLength)
}

// realmFactContextSource names where the served tier came from, in the same
// vocabulary the /models payload already uses (override | upstream | modelsdev
// | none) so one operator reads one term everywhere.
//
// The panel needs this to bold the value that is actually advertised. The
// served number and the provider's default tier are different numbers: with no
// override pinned, effectiveModelContext prefers the model's maximum capability
// (maxInputTokens / maxAllowedSize), so a model whose provider default is 200K
// is served as 1M. Bolding default_context_length therefore highlighted a tier
// nothing was using.
func realmFactContextSource(f modelFacts) string {
	if _, source, _ := effectiveModelContext(f.ID, derefInt64(f.ContextLength)); source != "" {
		return source
	}
	return "none"
}

func realmFactPanelValue(f modelFacts) map[string]any {
	return map[string]any{
		"status":                    "present",
		"present":                   true,
		"credits":                   map[string]any{"value": f.Credits, "rate": parseModelCreditsRate(f.Credits), "source": "snapshot", "status": map[bool]string{true: "reported", false: "unknown"}[f.Credits != ""]},
		"context_length":            realmFactEffectiveContextLength(f),
		"context_source":            realmFactContextSource(f),
		"default_context_length":    cloneInt64(f.DefaultContextLength),
		"supported_context_lengths": append([]int64(nil), f.SupportedContextLengths...),
		"max_completion_tokens":     cloneInt64(f.MaxCompletionTokens),
		"thinking":                  realmFactThinkingValue(f),
	}
}
