package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
)

const (
	installationSecretBytes   = 32
	installationIdentityMode  = "hmac-sha256-v1"
	installationIdentityDir   = ".workbuddy-installation-identity"
	installationIdentityReset = "offline_only"
)

var errInstallationIdentity = errors.New("installation identity unavailable")

type installationIdentityState struct {
	sync.RWMutex
	secret [installationSecretBytes]byte
	ready  bool
	scope  string // cleaned Host.AuthDir-derived directory; never exposed
}

var activeInstallationIdentity installationIdentityState

// configureInstallationIdentity observes the host's resolved AuthDir. Lifecycle
// register/reconfigure currently does not include HostConfigSummary, so this is
// called for each HostConfigSummary-bearing model/auth RPC and initializes on
// the first such request carrying AuthDir. Chat remains fail-closed until that happens; there is no ephemeral or
// user-global fallback.
func configureInstallationIdentity(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var req struct {
		Host struct {
			AuthDir      string
			AuthDirSnake string `json:"auth_dir"`
		}
	}
	if json.Unmarshal(raw, &req) != nil {
		return nil // the method-specific decoder reports malformed requests
	}
	authDir := req.Host.AuthDir
	if authDir == "" {
		authDir = req.Host.AuthDirSnake
	}
	if authDir == "" {
		return nil
	}
	if !filepath.IsAbs(authDir) {
		return errInstallationIdentity
	}
	return ensureInstallationIdentityForDir(filepath.Join(filepath.Clean(authDir), installationIdentityDir))
}

func ensureInstallationIdentityForDir(dir string) error {
	activeInstallationIdentity.Lock()
	defer activeInstallationIdentity.Unlock()
	if activeInstallationIdentity.ready {
		// Empty scope exists only in same-package offline tests that inject a key.
		if activeInstallationIdentity.scope == "" || activeInstallationIdentity.scope == dir {
			return nil
		}
		return errInstallationIdentity
	}
	secret, _, err := loadOrCreateInstallationSecret(dir, workBuddyEntropyReader{})
	if err != nil {
		return errInstallationIdentity
	}
	activeInstallationIdentity.secret = secret
	activeInstallationIdentity.ready = true
	activeInstallationIdentity.scope = dir
	return nil
}

func currentInstallationSecret() ([installationSecretBytes]byte, error) {
	activeInstallationIdentity.RLock()
	defer activeInstallationIdentity.RUnlock()
	if !activeInstallationIdentity.ready {
		return [installationSecretBytes]byte{}, errInstallationIdentity
	}
	return activeInstallationIdentity.secret, nil
}

func installationIdentityDiagnostics() map[string]any {
	activeInstallationIdentity.RLock()
	ready := activeInstallationIdentity.ready
	activeInstallationIdentity.RUnlock()
	storage := "unavailable"
	if ready {
		storage = "owner_only_auth_scope"
	}
	return map[string]any{
		"ready":      ready,
		"derivation": installationIdentityMode,
		"storage":    storage,
		"reset":      installationIdentityReset,
	}
}
