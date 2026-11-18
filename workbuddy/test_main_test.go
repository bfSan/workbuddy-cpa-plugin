package main

import (
	"os"
	"testing"
)

func installInstallationSecretForTest(secret [installationSecretBytes]byte) {
	activeInstallationIdentity.Lock()
	activeInstallationIdentity.secret = secret
	activeInstallationIdentity.ready = true
	activeInstallationIdentity.scope = ""
	activeInstallationIdentity.Unlock()
}

func clearInstallationSecretForTest() {
	activeInstallationIdentity.Lock()
	activeInstallationIdentity.secret = [installationSecretBytes]byte{}
	activeInstallationIdentity.ready = false
	activeInstallationIdentity.scope = ""
	activeInstallationIdentity.Unlock()
}

func TestMain(m *testing.M) {
	var secret [installationSecretBytes]byte
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	installInstallationSecretForTest(secret)
	os.Exit(m.Run())
}
