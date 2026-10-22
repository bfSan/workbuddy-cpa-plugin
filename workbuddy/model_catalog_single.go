// model_catalog_single.go re-discovers ONE account's model catalog and makes the
// host re-register that credential, from the panel's per-account 刷新 button.
//
// Why this exists (2026-10-22): the host only calls model.for_auth when a
// credential is (re-)registered. An account that stays unused for a long time --
// typically one parked at zero credits -- therefore keeps whatever catalog it
// registered with, even after upstream starts advertising new models for it.
// Observed live: workbuddy-67449c05 (bfengsan@gmail.com, global realm) had no
// bare "deepseek-v4.1-flash" in its registered catalog for days while every
// other account did, so a staged rule naming it in stage 1 silently skipped it
// and traffic fell through to stage 2 forever. Disabling and re-enabling the
// account fixed it instantly, which proved the catalog was stale rather than
// upstream omitting the model.
//
// The panel's 刷新 button used to refresh credits only, so the one button an
// operator reaches for could not fix that state. It now refreshes both.
//
// Two steps are required and neither alone is enough:
//  1. refreshModelCatalogForAuth re-pulls the upstream catalog into the plugin's
//     in-memory snapshot (what the panel renders).
//  2. Saving the auth file back (byte-identical storage) makes the host re-run
//     its registration path for that credential, which is the only moment it
//     calls model.for_auth again and so rebuilds the registry the router reads.
//
// Step 2 is a same-content write: it bumps mtime, which is what the host's
// watcher keys on. It deliberately does NOT toggle disabled, so the account never
// leaves the routing pool and there is no availability gap.
package main

import (
	"encoding/json"
	"strings"
)

// refreshAccountModelCatalog re-discovers one account's catalog and asks the host
// to re-register it. Best-effort by design: the caller is a UI refresh, and a
// failed upstream pull must not turn a working 刷新 button into an error.
//
// It returns a short human-readable status for the panel:
//
//	"refreshed"          catalog re-pulled and re-registration triggered
//	"catalog-only"       catalog re-pulled, but the host re-register step failed
//	"not-ready"          upstream pull failed and no earlier catalog is usable
func refreshAccountModelCatalog(authID, authIndex, callbackID string) string {
	runtime := activeModelRuntime.Load()
	if runtime == nil {
		return "not-ready"
	}
	previous := runtime.snapshotForAuthID(authID)
	snap, err := runtime.refreshModelCatalogForAuth(authID, authIndex, callbackID, previous)
	if err != nil && !snap.State.executable() {
		// Nothing usable was loaded (and no earlier snapshot to fall back on):
		// re-registering now would publish an empty catalog, which is worse than
		// leaving the last good registration alone.
		return "not-ready"
	}
	status := "refreshed"
	if err := triggerHostAuthReregister(authIndex); err != nil {
		status = "catalog-only"
	}
	return status
}

// triggerHostAuthReregister makes the host re-run registration for one credential
// by saving its auth file back unchanged.
//
// The bytes are re-encoded from the physical file rather than copied verbatim so
// the write goes through the same host.auth.save path every other plugin write
// uses (the host owns the file layout and may add fields). Every top-level key
// that is not part of the storage blob is preserved via buildAuthFileJSONFrom's
// base-inheritance, so disabled_reason / note / label survive the round-trip.
func triggerHostAuthReregister(authIndex string) error {
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil {
		return err
	}
	if phys == nil {
		return errNoPhysicalAuth
	}
	sa, err := parseStored(phys.JSON)
	if err != nil || sa == nil {
		return err
	}
	name := strings.TrimSpace(phys.Name)
	if name == "" {
		name = authFileNameFor(sa)
	}
	// Rebuild from the current document: only the storage blob is rewritten, the
	// metadata the host and this plugin already agreed on is inherited as-is.
	var base map[string]any
	if len(phys.JSON) > 0 {
		_ = json.Unmarshal(phys.JSON, &base)
	}
	disabled := false
	if raw, ok := base["disabled"].(bool); ok {
		disabled = raw
	}
	note, _ := base["note"].(string)
	raw, err := buildAuthFileJSONFrom(sa, disabled, note, nil, phys.JSON)
	if err != nil {
		return err
	}
	return hostAuthSaveJSONFn(name, raw)
}

// errNoPhysicalAuth reports a credential the host knows but cannot hand back as a
// file (runtime-only entries). Re-registration is impossible for those.
var errNoPhysicalAuth = errPhysicalAuthMissing{}

type errPhysicalAuthMissing struct{}

func (errPhysicalAuthMissing) Error() string { return "auth record has no physical file" }
