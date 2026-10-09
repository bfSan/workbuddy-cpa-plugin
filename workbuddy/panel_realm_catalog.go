package main

import "strings"

// panelRealmSelection is the one snapshot chosen for a realm. Keeping the
// snapshot and status together prevents the panel from presenting rows from
// one account/generation with status from another.
type panelRealmSelection struct {
	authID   string
	snapshot modelReadinessSnapshot
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
		// Keep failed/loading snapshots for realm_status. buildPanelRealmModels
		// separately requires an executable snapshot with facts, so a failed realm
		// is reported as failed rather than being mistaken for not_loaded.
		prior, ok := selected[snap.Realm]
		if !ok || panelRealmSnapshotBetter(snap, prior.snapshot) ||
			(panelRealmSnapshotEquivalent(snap, prior.snapshot) && file.ID < prior.authID) {
			selected[snap.Realm] = panelRealmSelection{authID: file.ID, snapshot: snap}
		}
	}
	return selected
}

// buildPanelRealmCatalog selects once for both response fields. It reads only
// auth IDs and published snapshots, never credentials or upstream endpoints.
func panelRealmSnapshotBetter(candidate, current modelReadinessSnapshot) bool {
	if candidate.executable() != current.executable() {
		return candidate.executable()
	}
	return candidate.ModelsFetchedAt.After(current.ModelsFetchedAt)
}

func panelRealmSnapshotEquivalent(a, b modelReadinessSnapshot) bool {
	return a.executable() == b.executable() && a.ModelsFetchedAt.Equal(b.ModelsFetchedAt)
}

func buildPanelRealmCatalog() ([]map[string]any, map[string]any) {
	selected := panelRealmSelections()
	return buildPanelRealmModels(selected), buildPanelRealmStatus(selected)
}

func buildPanelRealmModels(selected map[workBuddyRealm]panelRealmSelection) []map[string]any {
	byID := make(map[string]map[string]any)
	order := make([]string, 0)
	for _, realm := range []workBuddyRealm{workBuddyRealmCN, workBuddyRealmGlobal} {
		selection, loaded := selected[realm]
		if !loaded || !selection.snapshot.executable() || len(selection.snapshot.ModelFacts) == 0 {
			continue
		}
		for _, fact := range selection.snapshot.ModelFacts {
			id := strings.TrimSpace(fact.ID)
			if id == "" {
				continue
			}
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
		for _, realm := range []workBuddyRealm{workBuddyRealmCN, workBuddyRealmGlobal} {
			if _, ok := row[string(realm)]; ok {
				continue
			}
			if _, loaded := selected[realm]; loaded {
				row[string(realm)] = map[string]any{"status": "absent", "present": false}
			} else {
				row[string(realm)] = map[string]any{"status": "not_loaded", "present": nil}
			}
		}
		out = append(out, row)
	}
	return out
}

func buildPanelRealmStatus(selected map[workBuddyRealm]panelRealmSelection) map[string]any {
	status := make(map[string]any, 2)
	for _, realm := range []workBuddyRealm{workBuddyRealmCN, workBuddyRealmGlobal} {
		selection, loaded := selected[realm]
		if !loaded {
			status[string(realm)] = map[string]any{"status": "not_loaded", "loaded": false}
			continue
		}
		snap := selection.snapshot
		status[string(realm)] = map[string]any{
			"status":     string(snap.State),
			"loaded":     snap.executable(),
			"source":     string(snap.ModelSource),
			"fetched_at": snap.ModelsFetchedAt,
		}
	}
	return status
}

func realmFactPanelValue(f modelFacts) map[string]any {
	thinking := map[string]any{"status": "unknown", "levels": []string{}}
	return map[string]any{
		"status":                    "present",
		"present":                   true,
		"credits":                   map[string]any{"value": f.Credits, "rate": parseModelCreditsRate(f.Credits), "source": "snapshot", "status": map[bool]string{true: "reported", false: "unknown"}[f.Credits != ""]},
		"context_length":            cloneInt64(f.ContextLength),
		"default_context_length":    cloneInt64(f.DefaultContextLength),
		"supported_context_lengths": append([]int64(nil), f.SupportedContextLengths...),
		"max_completion_tokens":     cloneInt64(f.MaxCompletionTokens),
		"thinking":                  thinking,
	}
}
