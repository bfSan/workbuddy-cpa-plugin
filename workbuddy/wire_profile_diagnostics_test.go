package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestWireProfileDiagnosticsAreResolvedAndCredentialFree(t *testing.T) {
	oldFeatures := currentFeatureRuntime()
	t.Cleanup(func() { featureRuntime.Store(oldFeatures) })
	cfg, err := parseFeatureRuntime([]byte("wire_version_global_desktop: 6.1.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(cfg)

	base := loadedManagementBasePath() + "/plugins/" + providerName
	resp := managementResponseForTest(t, pluginapi.ManagementRequest{Method: http.MethodGet, Path: base + "/wire-profiles"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Profiles             []wireProfileDiagnostic `json:"profiles"`
		InstallationIdentity map[string]any          `json:"installation_identity"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Profiles) != 3 {
		t.Fatalf("profiles = %#v", payload.Profiles)
	}
	if got := payload.Profiles[2]; got.Realm != "global" || got.Profile != "workbuddy-macos-global" || got.Version != "6.1.2" || got.VersionSource != "config_override" {
		t.Fatalf("global diagnostic = %#v", got)
	}
	if len(payload.InstallationIdentity) != 4 || payload.InstallationIdentity["derivation"] != installationIdentityMode || payload.InstallationIdentity["ready"] != true || payload.InstallationIdentity["storage"] != "owner_only_auth_scope" || payload.InstallationIdentity["reset"] != installationIdentityReset {
		t.Fatalf("identity diagnostic = %#v", payload.InstallationIdentity)
	}
	var exact map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &exact); err != nil {
		t.Fatal(err)
	}
	if len(exact) != 2 || exact["profiles"] == nil || exact["installation_identity"] == nil {
		t.Fatalf("unexpected top-level diagnostic schema: %s", resp.Body)
	}
	var exactProfiles []map[string]any
	if err := json.Unmarshal(exact["profiles"], &exactProfiles); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]struct{}{"realm": {}, "client_mode": {}, "profile": {}, "version": {}, "version_source": {}}
	for _, profile := range exactProfiles {
		if len(profile) != len(allowed) {
			t.Fatalf("unexpected profile schema: %#v", profile)
		}
		for field := range profile {
			if _, ok := allowed[field]; !ok {
				t.Fatalf("unexpected profile field %q", field)
			}
		}
	}
	serialized := strings.ToLower(string(resp.Body))
	for _, forbidden := range []string{"access_token", "refresh_token", "authorization", "user_id", "enterprise_id", "session_id", "installation.key", "test_fixture"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("diagnostic exposed forbidden field/value %q: %s", forbidden, resp.Body)
		}
	}
}

func TestWireProfileDiagnosticsNeverMixConfigGenerations(t *testing.T) {
	old := currentFeatureRuntime()
	t.Cleanup(func() { featureRuntime.Store(old) })
	first, err := parseFeatureRuntime([]byte("wire_version_cli: 1.1.1\nwire_version_cn_desktop: 1.1.1\nwire_version_global_desktop: 1.1.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseFeatureRuntime([]byte("wire_version_cli: 2.2.2\nwire_version_cn_desktop: 2.2.2\nwire_version_global_desktop: 2.2.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(first)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			featureRuntime.Store(second)
			featureRuntime.Store(first)
		}
	}()
	var mixed []wireProfileDiagnostic
	for i := 0; i < 2000; i++ {
		profiles := resolvedWireProfileDiagnostics()
		if len(profiles) != 3 || profiles[0].Version != profiles[1].Version || profiles[1].Version != profiles[2].Version {
			mixed = profiles
			break
		}
	}
	wg.Wait()
	if mixed != nil {
		t.Fatalf("mixed config generations: %#v", mixed)
	}
}

func TestManagementRegistrationIncludesWireProfileDiagnosticsWithoutIdentityReset(t *testing.T) {
	want := "/plugins/" + providerName + "/wire-profiles"
	found := false
	for _, route := range managementRegistration().Routes {
		if route.Method == http.MethodGet && route.Path == want {
			found = true
		}
		if strings.Contains(strings.ToLower(route.Path), "identity") && strings.Contains(strings.ToLower(route.Path), "reset") {
			t.Fatalf("online installation identity reset route must not exist: %#v", route)
		}
	}
	if !found {
		t.Fatalf("missing GET %s", want)
	}
}
