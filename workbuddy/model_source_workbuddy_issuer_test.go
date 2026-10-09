package main

import "testing"

func TestWorkBuddyGlobalIssuerUsesExactHostAllowlist(t *testing.T) {
	accepted := []string{
		"https://workbuddy.ai/realms/cli",
		"https://www.workbuddy.ai/auth/realms/copilot",
	}
	for _, issuer := range accepted {
		t.Run("accept/"+issuer, func(t *testing.T) {
			got, err := workBuddyRealmFromAccessToken(syntheticAccessToken(t, issuer))
			if err != nil {
				t.Fatalf("issuer %q: %v", issuer, err)
			}
			if got != workBuddyRealmGlobal {
				t.Fatalf("issuer %q: realm = %q, want %q", issuer, got, workBuddyRealmGlobal)
			}
		})
	}

	rejected := []string{
		"https://api.workbuddy.ai/auth/realms/copilot",
		"https://evil.www.workbuddy.ai/auth/realms/copilot",
		"https://www.workbuddy.ai.evil.example/auth/realms/copilot",
	}
	for _, issuer := range rejected {
		t.Run("reject/"+issuer, func(t *testing.T) {
			if got, err := workBuddyRealmFromAccessToken(syntheticAccessToken(t, issuer)); err == nil {
				t.Fatalf("issuer %q: realm = %q, want unsupported-host error", issuer, got)
			}
		})
	}
}

func TestModelAuthIdentityRecognizesWWWGlobalIssuer(t *testing.T) {
	sa := syntheticStoredAuth(t, workBuddyRealmGlobal)
	sa.Auth.AccessToken = syntheticAccessToken(t, "https://www.workbuddy.ai/auth/realms/copilot")

	identity, err := modelAuthIdentityFor("auth-www-global", sa)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Provider != providerName || identity.Realm != workBuddyRealmGlobal {
		t.Fatalf("identity = %#v, want provider %q and global realm", identity, providerName)
	}
	if identity.UID != "uid-1" || identity.AuthID != "" {
		t.Fatalf("identity = %#v, want UID identity without AuthID", identity)
	}
}
