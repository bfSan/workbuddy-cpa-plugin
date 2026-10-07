package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestOAuthProfileUsesCorrectRealmPerRegion is the reason this change exists: the
// two realms are separate account systems with SEPARATE MODEL CATALOGS. A login
// that starts against the wrong realm mints a token whose iss belongs to the
// other side, and fetchWorkBuddyCatalog then pulls the wrong model list for the
// account forever. So the login endpoint and the Origin/Referer must both follow
// the requested region — not just one of them.
func TestOAuthProfileUsesCorrectRealmPerRegion(t *testing.T) {
	cn := oauthProfileForModeRegion(oauthClientModeWorkBuddy, oauthLoginRegionCN)
	if !strings.HasPrefix(cn.stateURL, upstreamBaseCN) {
		t.Errorf("CN stateURL = %q, want it on %s", cn.stateURL, upstreamBaseCN)
	}
	if strings.Contains(cn.origin, "workbuddy.ai") {
		t.Errorf("CN origin = %q, must not point at the international portal", cn.origin)
	}

	global := oauthProfileForModeRegion(oauthClientModeWorkBuddy, oauthLoginRegionGlobal)
	if !strings.HasPrefix(global.stateURL, upstreamBaseGlobal) {
		t.Errorf("Global stateURL = %q, want it on %s", global.stateURL, upstreamBaseGlobal)
	}
	if !strings.Contains(global.origin, "workbuddy.ai") {
		t.Errorf("Global origin = %q, want the international portal", global.origin)
	}

	if cn.stateURL == global.stateURL || cn.origin == global.origin {
		t.Error("the two realms must not collapse onto the same login endpoint")
	}
	// The desktop UA is a client identity, not a realm marker: it stays the same.
	if cn.userAgent != global.userAgent {
		t.Error("user agent must not change by region; it identifies the client, not the realm")
	}
}

// TestNormalizeOAuthLoginRegionDefaultsToCN pins the compatibility contract: any
// caller that does not send a region — an older panel, or a script built before
// the international realm existed — must keep logging into CN exactly as before.
func TestNormalizeOAuthLoginRegionDefaultsToCN(t *testing.T) {
	cases := map[string]string{
		"":              oauthLoginRegionCN,
		"cn":            oauthLoginRegionCN,
		"CN":            oauthLoginRegionCN,
		" cn ":          oauthLoginRegionCN,
		"global":        oauthLoginRegionGlobal,
		"GLOBAL":        oauthLoginRegionGlobal,
		"intl":          oauthLoginRegionGlobal,
		"international": oauthLoginRegionGlobal,
		"garbage":       oauthLoginRegionCN,
	}
	for in, want := range cases {
		if got := normalizeOAuthLoginRegion(in); got != want {
			t.Errorf("normalizeOAuthLoginRegion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOAuthStartRegionFromRequest(t *testing.T) {
	// Query form.
	req := pluginapi.ManagementRequest{Query: url.Values{"region": []string{"global"}}}
	if got := oauthStartRegionFromRequest(req); got != oauthLoginRegionGlobal {
		t.Errorf("query region = %q, want global", got)
	}
	// Body form.
	req = pluginapi.ManagementRequest{Body: []byte(`{"region":"global"}`)}
	if got := oauthStartRegionFromRequest(req); got != oauthLoginRegionGlobal {
		t.Errorf("body region = %q, want global", got)
	}
	// Absent: must stay CN, so pre-existing callers are unaffected.
	req = pluginapi.ManagementRequest{}
	if got := oauthStartRegionFromRequest(req); got != oauthLoginRegionCN {
		t.Errorf("absent region = %q, want cn", got)
	}
	// Malformed body must not panic or silently switch realms.
	req = pluginapi.ManagementRequest{Body: []byte(`{not json`)}
	if got := oauthStartRegionFromRequest(req); got != oauthLoginRegionCN {
		t.Errorf("malformed body = %q, want cn", got)
	}
}

// TestPanelCarriesBothLoginButtons pins the UI half: one entry per realm, so the
// operator picks the catalog they want instead of getting CN by default.
func TestPanelCarriesBothLoginButtons(t *testing.T) {
	html := string(servePanel(""))
	for _, needle := range []string{
		"登录 CN 账号",
		"登录国际账号",
		"openOAuthLogin(this,'cn')",
		"openOAuthLogin(this,'global')",
		"oauthRegion",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("panel is missing %q", needle)
		}
	}
	// The region must reach the API, not just set a label.
	if !strings.Contains(html, `body:JSON.stringify({region})`) {
		t.Error("the login request must send the chosen region")
	}
}

// TestLoginStepsStayInOneRealm is the regression behind "logged into an
// international account but the plugin never captured it".
//
// Login is three requests. The first (auth/state) was the only one made
// realm-aware when the international entry point was added; the other two still
// used the CN package constants. A Global login therefore opened the right page
// and minted a Global token, then asked copilot.tencent.com to honour it — which
// it will not — so /login/account failed and no account was ever written. The
// panel just kept polling.
func TestLoginStepsStayInOneRealm(t *testing.T) {
	cases := []struct {
		region   string
		wantBase string
	}{
		{oauthLoginRegionCN, upstreamBaseCN},
		{oauthLoginRegionGlobal, upstreamBaseGlobal},
	}
	for _, tc := range cases {
		p := oauthProfileForModeRegion(oauthClientModeWorkBuddy, tc.region)
		if p.base != tc.wantBase {
			t.Errorf("region %s: base = %q, want %q", tc.region, p.base, tc.wantBase)
		}
		// Every step must resolve onto that same base.
		if !strings.HasPrefix(p.stateURL, tc.wantBase) {
			t.Errorf("region %s: state URL = %q, want it on %s", tc.region, p.stateURL, tc.wantBase)
		}
		if got := endpointAuthTokenFor(p); !strings.HasPrefix(got, tc.wantBase) {
			t.Errorf("region %s: auth/token = %q, want it on %s", tc.region, got, tc.wantBase)
		}
		if got := endpointLoginAcctFor(p); !strings.HasPrefix(got, tc.wantBase) {
			t.Errorf("region %s: login/account = %q, want it on %s", tc.region, got, tc.wantBase)
		}
	}

	// The two realms must never resolve to the same host for any step.
	cn := oauthProfileForModeRegion(oauthClientModeWorkBuddy, oauthLoginRegionCN)
	gl := oauthProfileForModeRegion(oauthClientModeWorkBuddy, oauthLoginRegionGlobal)
	if endpointLoginAcctFor(cn) == endpointLoginAcctFor(gl) {
		t.Error("login/account collapses onto one host for both realms")
	}
	if endpointAuthTokenFor(cn) == endpointAuthTokenFor(gl) {
		t.Error("auth/token collapses onto one host for both realms")
	}
}

// TestCLIProfileKeepsHistoricalEndpoints guards the fallback: the CLI profile has
// one realm, and an empty base must keep the original CN constants so nothing
// that already worked changes.
func TestCLIProfileKeepsHistoricalEndpoints(t *testing.T) {
	// A profile built without a base (older shape, or constructed in tests).
	p := oauthRequestProfile{mode: oauthClientModeCLI}
	if got := endpointAuthTokenFor(p); got != endpointAuthToken {
		t.Errorf("empty base: auth/token = %q, want the CN default %q", got, endpointAuthToken)
	}
	if got := endpointLoginAcctFor(p); got != endpointLoginAcct {
		t.Errorf("empty base: login/account = %q, want the CN default %q", got, endpointLoginAcct)
	}
}
