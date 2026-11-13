package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestMacWorkBuddyVersionPins(t *testing.T) {
	// Read-only evidence from the installed application Info.plists on 2026-11-13:
	// com.tencent.workbuddy.mac=5.9.3, com.workbuddy.workbuddy-ai=5.9.4.
	if workBuddyCNVersion != "5.9.3" || workBuddyGlobalVersion != "5.9.4" {
		t.Fatalf("Mac version pins = CN %s / Global %s", workBuddyCNVersion, workBuddyGlobalVersion)
	}
	if loginVersionCN != workBuddyCNVersion || loginVersionGlobal != workBuddyGlobalVersion {
		t.Fatal("OAuth version pins drifted from the central wire profiles")
	}
	if workBuddyDesktopUACN != "WorkBuddy/5.9.3 WorkBuddy/5.9.3" {
		t.Fatalf("CN desktop UA = %q", workBuddyDesktopUACN)
	}
	if workBuddyDesktopUAGlobal != "WorkBuddy/5.9.4 WorkBuddy/5.9.4" {
		t.Fatalf("Global desktop UA = %q", workBuddyDesktopUAGlobal)
	}
	if codeBuddyCLIWireProfile.Platform != "CLI" {
		t.Fatalf("CLI login platform = %q", codeBuddyCLIWireProfile.Platform)
	}
}

func TestWireProfileForStoredAuthUsesPersistedClientFamily(t *testing.T) {
	cnDesktop := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeWorkBuddy}}
	cnCLI := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn", ClientMode: oauthClientModeCLI}}
	legacyCN := &storedAuth{Auth: storedTokens{Domain: "www.codebuddy.cn"}}
	global := &storedAuth{Auth: storedTokens{Domain: "www.workbuddy.ai", ClientMode: oauthClientModeCLI}}
	if got := wireProfileForStoredAuth(cnDesktop); got.Name != workBuddyCNWireProfile.Name {
		t.Fatalf("CN desktop profile = %#v", got)
	}
	if got := wireProfileForStoredAuth(cnCLI); got.Name != codeBuddyCLIWireProfile.Name {
		t.Fatalf("CN CLI profile = %#v", got)
	}
	if got := wireProfileForStoredAuth(legacyCN); got.Name != codeBuddyCLIWireProfile.Name {
		t.Fatalf("legacy CN profile = %#v", got)
	}
	if got := wireProfileForStoredAuth(global); got.Name != workBuddyGlobalWireProfile.Name {
		t.Fatalf("Global profile = %#v", got)
	}

	oldFeatures := featureRuntime.Load()
	desktop, err := parseFeatureRuntime([]byte("oauth_client_mode: workbuddy\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(desktop)
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })
	if got := wireProfileForStoredAuth(cnCLI); got.Name != codeBuddyCLIWireProfile.Name {
		t.Fatalf("global config changed persisted CN CLI profile: %#v", got)
	}
}

func TestWireProfilesNeverMixUAAndIDEFamily(t *testing.T) {
	for _, profile := range []workBuddyWireProfile{
		codeBuddyCLIWireProfile,
		workBuddyCNWireProfile,
		workBuddyGlobalWireProfile,
	} {
		req, err := http.NewRequest(http.MethodPost, "https://example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		applyWireProfileHeaders(req, profile)
		for key, want := range map[string]string{
			"User-Agent":    profile.UserAgent,
			"X-IDE-Type":    profile.IDEType,
			"X-IDE-Name":    profile.IDEName,
			"X-IDE-Version": profile.Version,
			"Origin":        profile.Origin,
			"Referer":       profile.Origin + "/",
		} {
			if got := req.Header.Get(key); got != want {
				t.Errorf("%s %s = %q, want %q", profile.Name, key, got, want)
			}
		}
		wantPrivate := ""
		if profile.Mode == oauthClientModeWorkBuddy {
			wantPrivate = "true"
		}
		if got := req.Header.Get("X-Private-Data"); got != wantPrivate {
			t.Errorf("%s X-Private-Data = %q, want %q", profile.Name, got, wantPrivate)
		}
	}
}

func TestLoginAndParsePersistClientModeAndRealm(t *testing.T) {
	cnToken := tokenData{
		AccessToken: syntheticAccessToken(t, "https://codebuddy.cn/realms/cli"),
		Domain:      "www.codebuddy.cn",
	}
	sa, err := buildLoginStoredAuth(cnToken, accountData{UID: "user-1"}, oauthClientModeWorkBuddy)
	if err != nil {
		t.Fatal(err)
	}
	if sa.Auth.ClientMode != oauthClientModeWorkBuddy {
		t.Fatalf("stored client mode = %q", sa.Auth.ClientMode)
	}
	raw, err := json.Marshal(sa)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseStored(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Auth.ClientMode != oauthClientModeWorkBuddy || wireProfileForStoredAuth(parsed).Name != workBuddyCNWireProfile.Name {
		t.Fatalf("parsed profile = %#v", parsed.Auth)
	}

	globalToken := tokenData{
		AccessToken: syntheticAccessToken(t, "https://workbuddy.ai/realms/cli"),
		Domain:      "www.workbuddy.ai",
	}
	global, err := buildLoginStoredAuth(globalToken, accountData{UID: "user-2"}, oauthClientModeCLI)
	if err != nil {
		t.Fatal(err)
	}
	if global.Auth.ClientMode != oauthClientModeWorkBuddy {
		t.Fatalf("Global mode = %q, want workbuddy", global.Auth.ClientMode)
	}
}

func TestParseStoredRejectsProfileAndRealmMismatch(t *testing.T) {
	cnToken := syntheticAccessToken(t, "https://codebuddy.cn/realms/cli")
	unknownIssuer := syntheticAccessToken(t, "https://unknown.example/realms/cli")
	httpIssuer := syntheticAccessToken(t, "http://codebuddy.cn/realms/cli")
	for _, raw := range [][]byte{
		[]byte(`{"auth":{"accessToken":"` + cnToken + `","domain":"www.codebuddy.cn","clientMode":"unknown"},"account":{"uid":"u"}}`),
		[]byte(`{"auth":{"accessToken":"` + cnToken + `","domain":"www.workbuddy.ai","clientMode":"workbuddy"},"account":{"uid":"u"}}`),
		[]byte(`{"auth":{"accessToken":"` + unknownIssuer + `","domain":"www.codebuddy.cn"},"account":{"uid":"u"}}`),
		[]byte(`{"auth":{"accessToken":"` + httpIssuer + `","domain":"www.codebuddy.cn"},"account":{"uid":"u"}}`),
		[]byte(`{"auth":{"accessToken":"a.***.c","domain":"www.codebuddy.cn"},"account":{"uid":"u"}}`),
		[]byte(`{"auth":{"accessToken":"opaque","domain":""},"account":{"uid":"u"}}`),
	} {
		if _, err := parseStored(raw); err == nil {
			t.Fatalf("unsafe auth parsed successfully: %s", raw)
		}
	}

	withoutDomain := []byte(`{"auth":{"accessToken":"` + cnToken + `"},"account":{"uid":"u"}}`)
	parsed, err := parseStored(withoutDomain)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Auth.Domain != "www.codebuddy.cn" || parsed.Auth.ClientMode != "" {
		t.Fatalf("normalized legacy auth = %#v", parsed.Auth)
	}
}

func TestRefreshedJWTLikeTokenRejectsInvalidIssuer(t *testing.T) {
	for _, token := range []string{
		syntheticAccessToken(t, "https://unknown.example/realms/cli"),
		syntheticAccessToken(t, "http://codebuddy.cn/realms/cli"),
		"a.***.c",
	} {
		sa := &storedAuth{Auth: storedTokens{AccessToken: token, Domain: "www.codebuddy.cn", ClientMode: oauthClientModeWorkBuddy}}
		if err := validateAndNormalizeStoredAuthRealm(sa); err == nil {
			t.Fatalf("refreshed JWT-like token accepted: %q", token)
		}
	}
}

func TestRefreshedGlobalRealmForcesWorkBuddyMode(t *testing.T) {
	sa := &storedAuth{Auth: storedTokens{
		AccessToken: syntheticAccessToken(t, "https://www.workbuddy.ai/auth/realms/copilot"),
		Domain:      "www.workbuddy.ai",
		ClientMode:  oauthClientModeCLI,
	}}
	if err := validateAndNormalizeStoredAuthRealm(sa); err != nil {
		t.Fatal(err)
	}
	if sa.Auth.ClientMode != oauthClientModeWorkBuddy {
		t.Fatalf("global mode = %q", sa.Auth.ClientMode)
	}
}

func TestGlobalRefreshUsesGlobalWorkBuddyProfile(t *testing.T) {
	sa := &storedAuth{
		Auth: storedTokens{
			Domain:       "www.workbuddy.ai",
			RefreshToken: "refresh-test",
		},
	}
	profile := oauthProfileForStoredAuth(sa)
	if profile.userAgent != workBuddyDesktopUAGlobal || profile.clientVersion != workBuddyGlobalVersion || profile.origin != originRefererGlobal {
		t.Fatalf("Global OAuth profile = %#v", profile)
	}
	req, err := buildTokenRefreshRequest(profile, sa)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"User-Agent": workBuddyDesktopUAGlobal,
		"Origin":     originRefererGlobal,
		"Referer":    originRefererGlobal + "/",
	} {
		if got := req.Header.Get(key); got != want {
			t.Errorf("Global refresh %s = %q, want %q", key, got, want)
		}
	}
	if req.URL.String() != upstreamBaseGlobal+"/v2/plugin/auth/token/refresh" {
		t.Fatalf("Global refresh URL = %s", req.URL)
	}
}
