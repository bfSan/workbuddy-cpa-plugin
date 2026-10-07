// management.go implements the WorkBuddy management API and web panel:
// account dashboard (nickname, credits, plan, check-in streak), manual/auto
// check-in (daily at 09:00 and 21:00 local time), and quota refresh.
package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type managementRequestWire struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// billingBase hosts the Buddy-gas-station check-in and resource-package APIs.
// It is a var (not const) so tests can override it with an httptest server.
var billingBase = "https://www.codebuddy.cn"

// billingBaseGlobal is the international (www.workbuddy.ai) billing base.
var billingBaseGlobal = "https://www.workbuddy.ai"

// If the panel later wants to surface "usage export ready", re-add it and wire
// it into buildDashboardEx's response.

// -----------------------------------------------------------------------------
// Account listing via host auth callbacks
// -----------------------------------------------------------------------------

type creditsSummary struct {
	// TotalRemain is currently usable credits across all active packages.
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed is consumed credits in the current cycle (sum of packages).
	TotalUsed int64 `json:"total_used"`
	// TotalSize is the credit capacity/pool (sum of package sizes). remain+used = size.
	TotalSize int64 `json:"total_size"`
	// TotalDosage is upstream's lifetime granted capacity: every package the
	// account has ever received, including long expired ones. It is NOT the
	// current pool and must never be subtracted from TotalRemain to derive spend
	// -- the two count different sets of packages, and doing so once produced
	// "used=99500" for an account whose packages summed to 4530.
	TotalDosage int64 `json:"total_dosage,omitempty"`
	// PackCount is number of resource packages included in the aggregate.
	PackCount int `json:"pack_count"`
	// FetchedAt is when this snapshot was taken (RFC3339). Upstream billing lag
	// can make remain/used look "stuck" for minutes after chat; compare this
	// timestamp — not only the numbers — when diagnosing frozen credits.
	FetchedAt string           `json:"fetched_at,omitempty"`
	Packages  []packageSummary `json:"packages"`
}

type packageSummary struct {
	Name       string `json:"name"`
	Remain     int64  `json:"remain"`
	Used       int64  `json:"used"`
	Size       int64  `json:"size"`
	CycleStart string `json:"cycle_start"`
	CycleEnd   string `json:"cycle_end"`
}

type checkinSummary struct {
	Active          bool     `json:"active"`
	TodayCheckedIn  bool     `json:"today_checked_in"`
	StreakDays      int64    `json:"streak_days"`
	DailyCredit     int64    `json:"daily_credit"`
	TodayCredit     int64    `json:"today_credit"`
	TotalCredits    int64    `json:"total_credits"`
	WeekCheckinDays int64    `json:"week_checkin_days"`
	ActivityName    string   `json:"activity_name"`
	Season          int64    `json:"season"`
	CheckinDates    []string `json:"checkin_dates,omitempty"`
}

// with a transient error (HTTP 5xx or transport error). codebuddy.cn
// intermittently returns 500s; without a retry a single hiccup surfaces as a
// panel error even though the very next request would succeed.
var billingRetryDelays = []time.Duration{300 * time.Millisecond, 900 * time.Millisecond}

// CapacityRemain/Used/Size         — lifetime package totals (Used often ≈0
//
//	for monthly-refresh free packs)
//
// CycleCapacityRemain/Used/Size    — the active billing cycle; Used is
//
//	sometimes omitted entirely
type resourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CapacitySize        int64  `json:"CapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleStartTime      string `json:"CycleStartTime"`
	CycleEndTime        string `json:"CycleEndTime"`
}

// -----------------------------------------------------------------------------
// Auto check-in timer (09:00 / 21:00 local)
// -----------------------------------------------------------------------------

// Management API routes + handler
// -----------------------------------------------------------------------------

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type resourceRoute struct {
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRegistrationResponse struct {
	Routes    []managementRoute `json:"routes,omitempty"`
	Resources []resourceRoute   `json:"resources,omitempty"`
}

// managementBasePathCache holds the host-injected BasePath so handleManagement
// doesn't hardcode /v0/management. Falls back to the historical default if the
// host doesn't provide one (older CPA builds).
var (
	managementBasePathCache   = "/v0/management"
	managementBasePathCacheMu sync.RWMutex
	resourceBasePathCache     = "/v0/resource/plugins/" + providerName
	resourceBasePathCacheMu   sync.RWMutex
)

func loadedManagementBasePath() string {
	managementBasePathCacheMu.RLock()
	defer managementBasePathCacheMu.RUnlock()
	return managementBasePathCache
}

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBasePathCacheMu.Lock()
	managementBasePathCache = p
	managementBasePathCacheMu.Unlock()
}

func loadedResourceBasePath() string {
	resourceBasePathCacheMu.RLock()
	defer resourceBasePathCacheMu.RUnlock()
	return resourceBasePathCache
}

func setResourceBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	resourceBasePathCacheMu.Lock()
	resourceBasePathCache = p
	resourceBasePathCacheMu.Unlock()
}

func managementRegistration() managementRegistrationResponse {
	base := "/plugins/" + providerName
	return managementRegistrationResponse{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: base + "/desensitize", Description: "Get effective WorkBuddy desensitize runtime settings."},
			{Method: http.MethodGet, Path: base + "/accounts", Description: "List WorkBuddy accounts with credits, plan and check-in status."},
			{Method: http.MethodGet, Path: base + "/egress-ip", Description: "Get the current egress IP through the active WorkBuddy HTTP route."},
			{Method: http.MethodPost, Path: base + "/refresh", Description: "Force refresh quota/cache for all accounts."},
			{Method: http.MethodPost, Path: base + "/checkin", Description: "Manually check in one account (auth_index) or all."},
			{Method: http.MethodPost, Path: base + "/checkin/config", Description: "Toggle auto check-in (enabled: true/false)."},
			{Method: http.MethodGet, Path: base + "/credits", Description: "Get real-time credits for one (auth_index query) or all accounts."},
			{Method: http.MethodPost, Path: base + "/import", Description: "Import WorkBuddy credential JSON (nested or flat) into host auth store."},
			{Method: http.MethodPost, Path: base + "/trial", Description: "Claim expert trial pack for one Global account (auth_index). One-time 250 credits / 14 days."},
			{Method: http.MethodPost, Path: base + "/keepalive", Description: "Manually refresh access tokens for all accounts (or one with auth_index)."},
			{Method: http.MethodGet, Path: base + "/keepalive/status", Description: "Last keepalive run summary + config."},
			{Method: http.MethodGet, Path: base + "/models", Description: "List the effective model catalog with its source and per-model cooldown state. Add ?refresh=1 to re-discover every account's catalog upstream instead of serving the cached snapshots."},
			{Method: http.MethodGet, Path: base + "/models/context", Description: "Get persistent per-model context-window overrides."},
			{Method: http.MethodPost, Path: base + "/models/context", Description: "Set or clear one per-model context-window override."},
			{Method: http.MethodPut, Path: base + "/models", Description: "Replace the model list overlay (hide/order/add)."},
			{Method: http.MethodPost, Path: base + "/models/action", Description: "Apply one model list edit: hide, restore, move or add."},
			{Method: http.MethodGet, Path: base + "/models/credits", Description: "List per-model credit multipliers with their source."},
			{Method: http.MethodPost, Path: base + "/models/credits", Description: "Pin or clear one model's credit multiplier (body: {model, credits})."},
			{Method: http.MethodGet, Path: base + "/cooldowns", Description: "List active per-(account, model) throttling entries."},
			{Method: http.MethodPost, Path: base + "/cooldowns/clear", Description: "Clear throttling for one account (auth_id) or one pair (auth_id + model)."},
			{Method: http.MethodPost, Path: base + "/oauth/start", Description: "Start an OAuth login flow and return the URL to open."},
			{Method: http.MethodPost, Path: base + "/oauth/poll", Description: "Poll one OAuth login flow (body or query: state)."},
			{Method: http.MethodPost, Path: base + "/accounts/rename", Description: "Set the display name of one account (body: {auth_index, name})."},
			{Method: http.MethodPost, Path: base + "/accounts/delete", Description: "Delete one account so it can be re-registered (body: {auth_index})."},
			{Method: http.MethodPost, Path: base + "/accounts/disabled", Description: "Manually enable or disable one account (body: {auth_index, disabled}). Manual state is not recorded as a disable reason, so creditors automation will not silently override it."},
			{Method: http.MethodGet, Path: base + "/models/refresh-status", Description: "Last nightly upstream catalog refresh (per-model credit multipliers) plus its schedule and on/off state."},
			{Method: http.MethodPost, Path: base + "/models/refresh-status", Description: "Trigger the same upstream catalog refresh on demand; returns the run summary."},
		},
		Resources: []resourceRoute{
			{Path: "/panel", Menu: "WorkBuddy", Description: "WorkBuddy dashboard: credits, check-in, plan, import."},
		},
	}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Browser UI resource routes (unauthenticated).
	resPrefix := loadedResourceBasePath()
	if req.Method == http.MethodGet && strings.HasPrefix(path, resPrefix) {
		sub := strings.TrimPrefix(path, resPrefix)
		return okEnvelope(mgmtHTMLResponse(servePanel(sub)))
	}

	// Plugin-layer auth applies to every management API route when a key is
	// configured. Static panel resources return above so the login UI remains
	// reachable; the panel sends the Bearer key on each JSON request.
	mutating := req.Method == http.MethodPost || mutatingManagementPath(path)
	if loadedManagementKey() != "" || mutating {
		if status, msg := checkManagementAuth(req.ManagementRequest); status != 0 {
			ip := managementClientIP(req.ManagementRequest)
			if !allowManagementRequest(ip) {
				return okEnvelope(mgmtJSONResponse(http.StatusTooManyRequests, map[string]any{
					"error": "rate limit exceeded, try again later",
				}))
			}
			return okEnvelope(mgmtJSONResponse(status, map[string]any{"error": msg}))
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch {
	case req.Method == http.MethodGet && path == base+"/desensitize":
		cfg := currentFeatureRuntime()
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{
			"enabled": cfg.desensitizeEnabled,
			"terms":   append([]string(nil), cfg.desensitizeTerms...),
			"source":  cfg.desensitizeSource,
		}))
	case req.Method == http.MethodGet && path == base+"/accounts":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardExWithCallback(false, false, req.HostCallbackID)))
	case req.Method == http.MethodGet && path == base+"/egress-ip":
		ip, err := fetchEgressIPWithCallback(req.HostCallbackID)
		if err != nil {
			return okEnvelope(mgmtJSONResponse(http.StatusBadGateway, map[string]any{
				"error": "egress IP unavailable",
			}))
		}
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{"ip": ip}))
	case req.Method == http.MethodPost && path == base+"/refresh":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, buildDashboardExWithCallback(true, true, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/checkin":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleManualCheckinWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/checkin/config":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCheckinConfig(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCreditsQueryWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/import":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleImportAuth(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/trial":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleClaimTrialWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodPost && path == base+"/keepalive":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveNowWithCallback(req.ManagementRequest, req.HostCallbackID)))
	case req.Method == http.MethodGet && path == base+"/keepalive/status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleKeepaliveStatus()))
	case req.Method == http.MethodGet && path == base+"/models":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelListQueryForce(req.Query.Get("refresh") != "", req.HostCallbackID)))
	case req.Method == http.MethodPut && path == base+"/models":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelOverlayWrite(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/models/action":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelOverlayAction(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/models/context":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, map[string]any{"model_context": currentModelContextOverrides()}))
	case (req.Method == http.MethodPost || req.Method == http.MethodPut) && path == base+"/models/context":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelContextWrite(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/models/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelCreditsQuery()))
	case req.Method == http.MethodPost && path == base+"/models/credits":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleModelCreditsWrite(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/cooldowns":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCooldownList(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/cooldowns/clear":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCooldownClear(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/oauth/start":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleOAuthStart()))
	case req.Method == http.MethodPost && path == base+"/oauth/poll":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleOAuthPoll(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/accounts/rename":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleAccountRename(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/accounts/delete":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleAccountDelete(req.ManagementRequest)))
	case req.Method == http.MethodPost && path == base+"/accounts/disabled":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleAccountSetDisabled(req.ManagementRequest)))
	case req.Method == http.MethodGet && path == base+"/models/refresh-status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCatalogRefresh(false)))
	case req.Method == http.MethodPost && path == base+"/models/refresh-status":
		return okEnvelope(mgmtJSONResponse(http.StatusOK, handleCatalogRefresh(true)))
	}
	return okEnvelope(mgmtJSONResponse(http.StatusNotFound, map[string]any{"error": "not found: " + path}))
}

func handleModelContextWrite(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Model         string `json:"model"`
		ID            string `json:"id"`
		ContextLength *int64 `json:"context_length"`
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
	if body.ContextLength != nil && (*body.ContextLength <= 0 || *body.ContextLength > maxContextWindowValue) {
		return map[string]any{"success": false, "error": "context_length is out of range"}
	}
	// Reject a tier the provider does not offer. Guessing is what makes the
	// upstream fall back to its own smaller limit, which is the bug this
	// setting exists to prevent.
	if body.ContextLength != nil {
		if meta, known := contextMetadataForModel(id); known && len(meta.SupportedLengths) > 0 {
			offered := false
			for _, value := range meta.SupportedLengths {
				if value == *body.ContextLength {
					offered = true
					break
				}
			}
			if !offered {
				return map[string]any{
					"success":         false,
					"error":           "context_length is not a supported tier for this model",
					"context_options": meta.SupportedLengths,
				}
			}
		}
	}
	next := setModelContextOverride(id, body.ContextLength)
	// Snapshots bake the effective length in, so apply the new state to them
	// immediately; otherwise the previous tier stays visible to clients until the
	// next upstream refresh.
	applied := currentModelRuntime().reapplyModelContextSnapshots(id)
	return map[string]any{
		"success":          true,
		"model":            id,
		"context_length":   body.ContextLength,
		"model_context":    next,
		"persistent":       true,
		"snapshot_updated": applied,
	}
}

// handleAccountRename sets the display name of one credential.
//
// The name lives in account.nickname, which labelForAuth already reads when CPA
// renders the credential card. It deliberately does not touch the note: that
// field is rewritten on every credits refresh and would lose a custom label.
func handleAccountRename(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Name      string `json:"name"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.Query.Get("auth_index"))
	}
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return map[string]any{"error": "account not found"}
	}
	sa, err := parseStored(phys.JSON)
	if err != nil || sa == nil {
		return map[string]any{"error": "stored auth is nil"}
	}
	sa.Account.Nickname = strings.TrimSpace(body.Name)
	note := displayNoteWithPrev(sa, nil, phys.Disabled, existingNoteCredits(authIndex))
	// Renaming must not disturb the parked state: pass the live file so
	// disabled_reason survives (a manual park would otherwise be forgotten and
	// undone by the next reconcile).
	raw, err := buildAuthFileJSONFrom(sa, phys.Disabled, note, nil, phys.JSON)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := hostAuthSaveJSONFn(strings.TrimSpace(phys.Name), raw); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := waitForRuntimeAuthLabel(authIndex, labelForAuth(sa), 3*time.Second); err != nil {
		return map[string]any{"status": "ok", "auth_index": authIndex, "name": sa.Account.Nickname, "warning": err.Error()}
	}
	return map[string]any{"status": "ok", "auth_index": authIndex, "name": sa.Account.Nickname}
}

// handleAccountSetDisabled turns one credential on or off by hand.
//
// Why this exists: the plugin decides disabled purely from credits, and that
// automation is deliberately conservative (see lifecycleActionFor). An operator
// still needs a direct switch — to park a credential that is misbehaving without
// deleting it, or to bring one back that automation left parked. Without this
// the only knobs were "wait for reconcile" and "delete and re-login".
//
// The manual decision IS persisted, in disabled_reason = "manual". That is what
// makes it durable: on restart the plugin re-reads every auth file, and
// shouldReenableCN refuses to revive an account carrying this reason, so a
// disabled account comes back disabled. A manual ENABLE clears the field, which
// is the only way the state returns to automation's control.
//
// Note the asymmetry with the failure reasons: "exhausted" itself now means
// "revive me" (that reason was retired), while "manual" means "leave me alone".
// Stamping a manual disable as "exhausted" would have had the opposite effect.
func handleAccountSetDisabled(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
		Disabled  *bool  `json:"disabled"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.Query.Get("auth_index"))
	}
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	disabled := body.Disabled
	if disabled == nil {
		// Fall back to a query/body scalar so the switch also works as a GET-style
		// toggle (?disabled=true).
		raw := strings.TrimSpace(req.Query.Get("disabled"))
		if raw == "" {
			return map[string]any{"error": "disabled is required"}
		}
		val := raw == "true" || raw == "1"
		disabled = &val
	}
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return map[string]any{"error": "account not found"}
	}
	sa, err := parseStored(phys.JSON)
	if err != nil || sa == nil {
		return map[string]any{"error": "stored auth is nil"}
	}
	next := *disabled
	note := displayNoteWithPrev(sa, nil, next, existingNoteCredits(authIndex))
	extra := map[string]any{}
	if next {
		// ★ Persist the manual intent. This is what makes the state survive a
		// restart: on reload the plugin re-reads each auth file, and this field
		// tells shouldReenableCN that the parked state was deliberate. Without it
		// (e.g. writing null) the reason reads back empty, the legacy fallback
		// sees a readable balance and no dead-session marker, and reconcile
		// re-enables the account — the operator's disable would look like it
		// worked, then quietly undo itself after the next tick or restart.
		extra["disabled_reason"] = disableReasonManual
	} else {
		// Enabling is the explicit undo: clear the reason so it cannot be read as
		// a stale failure later. null removes the key entirely.
		extra["disabled_reason"] = nil
	}
	raw, err := buildAuthFileJSONFrom(sa, next, note, extra, phys.JSON)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if err := hostAuthSaveJSONFn(strings.TrimSpace(phys.Name), raw); err != nil {
		return map[string]any{"error": err.Error()}
	}
	// The cached dashboard must not keep showing the old state.
	accountCache.Delete(authIndex)
	if id := strings.TrimSpace(sa.Account.UID); id != "" {
		accountCache.Delete(id)
	}
	// The host only re-reads the auth file when its watcher fires; wait briefly so
	// the panel's immediate reload reflects the change instead of a stale label.
	warning := ""
	if err := waitForRuntimeAuthLabel(authIndex, labelForAuth(sa), 3*time.Second); err != nil {
		warning = err.Error()
	}
	out := map[string]any{
		"status":     "ok",
		"auth_index": authIndex,
		"disabled":   next,
		"label":      labelForAuth(sa),
	}
	if warning != "" {
		out["warning"] = warning
	}
	return out
}

// handleAccountDelete removes one credential so a stuck account can be
// re-registered with a fresh login. The plugin SDK has no auth.delete method,
// so this drops the physical file through the same guarded helper the
// lifecycle migration uses (absolute path, confined to the auth directory).
func handleAccountDelete(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if len(req.Body) > 0 {
		_ = json.Unmarshal(req.Body, &body)
	}
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		authIndex = strings.TrimSpace(req.Query.Get("auth_index"))
	}
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	phys, err := hostAuthGetPhysicalFn(authIndex)
	if err != nil || phys == nil {
		return map[string]any{"error": "account not found"}
	}
	path := strings.TrimSpace(phys.Path)
	if path == "" {
		return map[string]any{"error": "auth file path unavailable"}
	}
	if err := deleteAuthFileInDir(path, filepath.Dir(path)); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"status": "ok", "auth_index": authIndex, "file": filepath.Base(path)}
}

// -----------------------------------------------------------------------------
// Plugin-layer management auth + rate limit (v0.6.31)
// -----------------------------------------------------------------------------
//
// When management_key is configured (config_yaml or WB_MANAGEMENT_KEY env), all
// management API routes under /v0/management/plugins/workbuddy/* require a
// matching Bearer token. Static panel resources stay public so the UI can load
// and prompt for the key; the panel itself supplies the key on every API call.
//
// A per-IP token-bucket rate limiter guards against brute-force when the key
// check fails repeatedly.

const (
	mgmtRateLimitCapacity = 5                // burst
	mgmtRateLimitRefill   = time.Minute / 10 // 1 token per 6s
	mgmtRateLimitTTL      = 10 * time.Minute // idle entry eviction
)

type mgmtRateEntry struct {
	tokens   float64
	lastSeen time.Time
}

var (
	mgmtRateLimit   = map[string]*mgmtRateEntry{}
	mgmtRateLimitMu sync.Mutex
)

func loadedManagementKey() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

// checkManagementAuth returns an HTTP status + error message when the request
// should be rejected. status=0 means allow.
func checkManagementAuth(req pluginapi.ManagementRequest) (int, string) {
	want := loadedManagementKey()
	if want == "" {
		return 0, "" // plugin-layer auth disabled; rely on host middleware
	}
	got := strings.TrimSpace(req.Headers.Get("Authorization"))
	if !strings.HasPrefix(got, "Bearer ") {
		return http.StatusUnauthorized, "missing Bearer token"
	}
	token := strings.TrimSpace(strings.TrimPrefix(got, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(token), []byte(want)) != 1 {
		return http.StatusForbidden, "invalid management key"
	}
	return 0, ""
}

// allowManagementRequest applies a per-IP token bucket. ip may be empty when the
// host doesn't forward X-Forwarded-For / RemoteAddr — in that case use a single
// global bucket.
func allowManagementRequest(ip string) bool {
	if ip == "" {
		ip = "_global"
	}
	mgmtRateLimitMu.Lock()
	defer mgmtRateLimitMu.Unlock()
	now := time.Now()
	e, ok := mgmtRateLimit[ip]
	if !ok {
		e = &mgmtRateEntry{tokens: mgmtRateLimitCapacity, lastSeen: now}
		mgmtRateLimit[ip] = e
	}
	// Refill.
	elapsed := now.Sub(e.lastSeen)
	e.tokens += float64(elapsed) / float64(mgmtRateLimitRefill)
	if e.tokens > mgmtRateLimitCapacity {
		e.tokens = mgmtRateLimitCapacity
	}
	e.lastSeen = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	// Lazy eviction of idle entries (don't grow the map forever).
	if len(mgmtRateLimit) > 1024 {
		for k, v := range mgmtRateLimit {
			if now.Sub(v.lastSeen) > mgmtRateLimitTTL {
				delete(mgmtRateLimit, k)
			}
		}
	}
	return true
}

// managementClientIP extracts a best-effort client identifier for rate limiting.
// CPA host doesn't currently forward RemoteAddr, so fall back to X-Forwarded-For
// / X-Real-IP headers if the deployment adds them via a reverse proxy.
func managementClientIP(req pluginapi.ManagementRequest) string {
	if xff := strings.TrimSpace(req.Headers.Get("X-Forwarded-For")); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	if xr := strings.TrimSpace(req.Headers.Get("X-Real-Ip")); xr != "" {
		return xr
	}
	return ""
}

// mutatingManagementPath reports whether the path performs a write (checkin,
// import, trial claim, refresh, config toggle). Read endpoints pass.
func mutatingManagementPath(path string) bool {
	base := loadedManagementBasePath() + "/plugins/" + providerName
	switch path {
	case base + "/refresh",
		base + "/checkin",
		base + "/checkin/config",
		base + "/import",
		base + "/trial",
		base + "/keepalive",
		base + "/models",
		base + "/models/action",
		base + "/models/context",
		base + "/models/refresh-status",
		base + "/accounts/disabled":
		return true
	}
	return false
}

func mgmtJSONResponse(status int, v any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json; charset=utf-8")
	return pluginapi.ManagementResponse{StatusCode: status, Headers: h, Body: body}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	h := http.Header{}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	return pluginapi.ManagementResponse{StatusCode: http.StatusOK, Headers: h, Body: body}
}

// checkinLocks serializes per-account manual check-in (B4).
// Entries are pruned during dashboard prune to avoid unbounded growth
// when auth accounts are deleted/rotated.
var (
	checkinLocks sync.Map // auth_index -> *sync.Mutex
)
