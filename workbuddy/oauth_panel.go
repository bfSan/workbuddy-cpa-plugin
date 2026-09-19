// oauth_panel.go exposes the existing OAuth flow through the plugin's
// management API so the panel can drive it.
//
// The flow itself already exists for CPA's auth.login.start / auth.login.poll
// RPCs, which the host drives from its own auth page. That path is unavailable
// to an embedded panel, which is why signing in from the panel meant pasting a
// credential JSON by hand. These endpoints reuse the exact same login state
// machine — same cookie isolation, same TTL, same completion path — so a
// panel-started login is not a second implementation.
//
// The login URL is opened by the operator's browser, not fetched by the
// plugin, so the browser needs its own route to the upstream.
package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleOAuthStart opens a login flow and returns the URL for the operator to
// open. The state token is what the panel polls with; it is single-use and
// expires with the flow.
func handleOAuthStart() map[string]any {
	raw, err := handleStartLogin(nil)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	var env struct {
		OK     bool                             `json:"ok"`
		Result pluginapi.AuthLoginStartResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return map[string]any{"success": false, "error": "login start failed"}
	}
	return map[string]any{
		"success":   true,
		"url":       env.Result.URL,
		"state":     env.Result.State,
		"expiresAt": env.Result.ExpiresAt.UTC().Format(time.RFC3339),
		"expiresIn": int(time.Until(env.Result.ExpiresAt).Round(time.Second) / time.Second),
	}
}

// handleOAuthPoll drives one poll of an in-flight flow. It is single-shot by
// design — the panel owns the cadence — and returns pending until the operator
// finishes in the browser.
func handleOAuthPoll(req pluginapi.ManagementRequest) map[string]any {
	state := strings.TrimSpace(req.Query.Get("state"))
	if state == "" && len(req.Body) > 0 {
		var body struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil {
			state = strings.TrimSpace(body.State)
		}
	}
	if state == "" {
		return map[string]any{"success": false, "error": "state is required"}
	}
	payload, err := json.Marshal(pluginapi.AuthLoginPollRequest{Provider: providerName, State: state})
	if err != nil {
		return map[string]any{"success": false, "error": "encode poll request"}
	}
	raw, err := handlePollLogin(payload)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	var env struct {
		OK     bool                            `json:"ok"`
		Result pluginapi.AuthLoginPollResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		return map[string]any{"success": false, "error": "login poll failed"}
	}
	status := string(env.Result.Status)
	// The credential is already handed to the host on success; the panel only
	// needs to know it can reload, so no auth payload is echoed back.
	nickname := ""
	if env.Result.Auth.StorageJSON != nil && len(env.Result.Auth.StorageJSON) > 0 {
		var sa storedAuth
		if err := json.Unmarshal(env.Result.Auth.StorageJSON, &sa); err == nil {
			nickname = strings.TrimSpace(sa.Account.Nickname)
		}
	}
	return map[string]any{
		"success":  status == string(pluginapi.AuthLoginStatusSuccess),
		"status":   status,
		"message":  env.Result.Message,
		"nickname": nickname,
		"done":     status != string(pluginapi.AuthLoginStatusPending),
	}
}
