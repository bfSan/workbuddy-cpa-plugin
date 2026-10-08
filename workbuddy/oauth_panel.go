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
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleOAuthStart opens a login flow and returns the URL for the operator to
// open. The state token is what the panel polls with; it is single-use and
// expires with the flow.
// oauthStartRegionFromRequest reads the realm a NEW login should target.
// Accepted as ?region= or {"region": ...}; absent means CN, which is what every
// caller before the international realm existed already sends.
func oauthStartRegionFromRequest(req pluginapi.ManagementRequest) string {
	if v := strings.TrimSpace(req.Query.Get("region")); v != "" {
		return normalizeOAuthLoginRegion(v)
	}
	if len(req.Body) > 0 {
		var body struct {
			Region string `json:"region"`
		}
		if err := json.Unmarshal(req.Body, &body); err == nil && strings.TrimSpace(body.Region) != "" {
			return normalizeOAuthLoginRegion(body.Region)
		}
	}
	return oauthLoginRegionCN
}

func handleOAuthStart(req pluginapi.ManagementRequest) map[string]any {
	region := oauthStartRegionFromRequest(req)
	raw, err := startLoginWithModeRegion(oauthClientModeWorkBuddy, region)
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
	// ★ The panel must not navigate to "url" as-is.
	//
	// The host HTML-escapes every string in a plugin management response before it
	// reaches the browser (internal/pluginhost calls htmlsanitize.JSONBodyIfLikely,
	// which runs html.EscapeString over the whole JSON document). A URL therefore
	// arrives with its query separators rewritten from & to the literal characters
	// "&amp;", and the browser reads that as a single nameless parameter: the state
	// never reaches the login page, which lands on /login/started with an empty
	// state and renders "Login Failed".
	//
	// Escaping inside a value is not something the plugin can switch off, so the
	// panel is given the pieces instead: url_base carries no separator at all, and
	// the parameters travel as a JSON object whose values are plain tokens (a
	// platform name, a UUID state, a version) containing nothing the escaper
	// touches. URLSearchParams re-encodes them on the way back out.
	//
	// "url" is kept for compatibility with anything still reading it, and the panel
	// falls back to it — unescaping &amp; itself — when the pieces are absent.
	base, params := splitLoginURL(env.Result.URL)
	return map[string]any{
		"success":   true,
		"url":       env.Result.URL,
		"url_base":  base,
		"query":     params,
		"state":     env.Result.State,
		"expiresAt": env.Result.ExpiresAt.UTC().Format(time.RFC3339),
		"expiresIn": int(time.Until(env.Result.ExpiresAt).Round(time.Second) / time.Second),
		// Echoed so the panel can show which realm the opened page belongs to and
		// so a silent fallback to CN is visible instead of assumed.
		"region": region,
	}
}

// splitLoginURL separates a login URL into a query-free base and its parameters.
//
// Returning the parts rather than one string lets the panel rebuild the URL with
// URLSearchParams, which survives the host's HTML escaping of response strings —
// see handleOAuthStart for why that matters. A URL that cannot be parsed comes
// back with an empty base, and the caller falls back to the raw string.
func splitLoginURL(raw string) (string, map[string]string) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", nil
	}
	params := make(map[string]string, len(u.Query()))
	for key, values := range u.Query() {
		if len(values) > 0 {
			params[key] = values[0]
		}
	}
	base := *u
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), params
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
