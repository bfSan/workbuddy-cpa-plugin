package main

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// pollOAuthPending is single-shot, never an unbounded background worker. The
// current invocation finishes even if the browser disconnects: its HTTP client
// is not tied to the management request context. No request-scoped host callback
// is retained or invoked after the plugin call returns.
func pollOAuthPending(state string, panel bool) (pluginapi.AuthLoginPollResponse, bool, time.Time, error) {
	var response pluginapi.AuthLoginPollResponse
	if !validOAuthState(state) {
		return response, false, time.Time{}, errors.New("poll: invalid or empty state")
	}
	lc, err := lookupOAuthPending(state)
	if err != nil {
		return response, false, time.Time{}, err
	}
	lc.mu.Lock()
	defer lc.mu.Unlock()
	persistent := lc.store != nil
	if !time.Now().Before(lc.expires) {
		removeOAuthPending(state, lc)
		return response, persistent, lc.expires, errors.New("poll: login expired; please restart login")
	}
	if lc.profile.mode == "" {
		lc.profile = oauthProfileForMode(oauthClientModeCLI)
	}
	if lc.created.IsZero() {
		lc.created = time.Now()
	}

	if lc.token == nil {
		client, err := loginClientForCurrentRouting(lc)
		if err != nil {
			retainOAuthResult(state, lc) // recoverable, but not actively polling
			return response, persistent, lc.expires, err
		}
		request, err := buildAuthTokenRequest(lc.profile, state)
		if err != nil {
			return response, persistent, lc.expires, oauthLoginFailure("token request failed", 0)
		}
		raw, status, tokenErr := doJSONRequest(client, request)
		if tokenErr != nil {
			if err := persistOAuthPending(state, lc); err != nil {
				return response, persistent, lc.expires, err
			}
			if status == 0 || status >= 500 {
				return response, persistent, lc.expires, oauthLoginFailure("token endpoint error", status)
			}
			response.Status, response.Message = pluginapi.AuthLoginStatusPending, "waiting for login"
			return response, persistent, lc.expires, nil
		}
		var token tokenData
		if json.Unmarshal(raw, &token) != nil || token.AccessToken == "" {
			if err := persistOAuthPending(state, lc); err != nil {
				return response, persistent, lc.expires, err
			}
			response.Status, response.Message = pluginapi.AuthLoginStatusPending, "waiting for login"
			return response, persistent, lc.expires, nil
		}
		lc.token, lc.tokenAt = &token, time.Now()
		lc.expires = lc.tokenAt.Add(oauthResultTTL)
		if hardLimit := lc.created.Add(loginTTL + oauthResultTTL); lc.expires.After(hardLimit) {
			lc.expires = hardLimit
		}
		retainOAuthResult(state, lc)
	}
	// Checkpoint token BEFORE account lookup. If this fails, leave the token in
	// memory; retries must first checkpoint it, never call auth/token again.
	if lc.result == nil {
		if err := persistOAuthPending(state, lc); err != nil {
			return response, persistent, lc.expires, err
		}
		client, err := loginClientForCurrentRouting(lc)
		if err != nil {
			return response, persistent, lc.expires, err
		}
		request, err := buildLoginAccountRequest(lc.profile, state, lc.token.AccessToken)
		if err != nil {
			return response, persistent, lc.expires, oauthLoginFailure("account request failed", 0)
		}
		raw, status, err := doJSONRequest(client, request)
		if err != nil {
			if checkpointErr := persistOAuthPending(state, lc); checkpointErr != nil {
				return response, persistent, lc.expires, checkpointErr
			}
			return response, persistent, lc.expires, oauthLoginFailure("account lookup failed after token success", status)
		}
		var account accountData
		if json.Unmarshal(raw, &account) != nil {
			return response, persistent, lc.expires, oauthLoginFailure("account lookup parse failed", 0)
		}
		sa, err := buildLoginStoredAuth(*lc.token, account)
		if err != nil {
			return response, persistent, lc.expires, err
		}
		// Absolute token expiry is anchored to receipt, not a later recovery poll.
		if lc.token.ExpiresIn > 0 {
			sa.Auth.ExpiresAt = lc.tokenAt.Add(time.Duration(lc.token.ExpiresIn) * time.Second).Unix()
		}
		lc.result = sa
	}
	if err := persistOAuthPending(state, lc); err != nil {
		return response, persistent, lc.expires, err
	}
	if panel && !lc.saved {
		fileJSON, err := buildAuthFileJSON(lc.result, false, displayNote(lc.result, nil, false), nil)
		if err != nil {
			return response, persistent, lc.expires, errors.New("login succeeded but the credential could not be encoded")
		}
		if err := hostAuthSaveJSONFn(authFileNameFor(lc.result), fileJSON); err != nil {
			// The host may include credential contents in its error. Never forward it.
			return response, persistent, lc.expires, errors.New("login succeeded but saving the account failed; retry this login")
		}
		lc.saved = true
		if err := persistOAuthPending(state, lc); err != nil {
			return response, persistent, lc.expires, err
		}
	}
	response.Status = pluginapi.AuthLoginStatusSuccess
	response.Auth = toAuthDataForLoginPoll(lc.result)
	return response, persistent, lc.expires, nil
}
