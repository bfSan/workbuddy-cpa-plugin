//go:build !unix

package main

import "io"

// Non-Unix platforms fail closed until an implementation can enforce equivalent
// owner-only ACLs, no-follow reads, inter-process locking and atomic persistence.
func loadOrCreateInstallationSecret(string, io.Reader) ([installationSecretBytes]byte, bool, error) {
	return [installationSecretBytes]byte{}, false, errInstallationIdentity
}
