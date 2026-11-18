//go:build unix

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

type failingEntropyReader struct{}

func (failingEntropyReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func restoreInstallationIdentityFixture(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		var fixture [installationSecretBytes]byte
		for i := range fixture {
			fixture[i] = byte(i + 1)
		}
		installInstallationSecretForTest(fixture)
	})
}

func TestInstallationIdentityBindsHostAuthDirOnce(t *testing.T) {
	restoreInstallationIdentityFixture(t)
	clearInstallationSecretForTest()
	firstAuthDir := t.TempDir()
	raw := []byte(`{"host":{"auth_dir":"` + firstAuthDir + `"}}`)
	if err := configureInstallationIdentity(raw); err != nil {
		t.Fatal(err)
	}
	secret, err := currentInstallationSecret()
	if err != nil || secret == ([installationSecretBytes]byte{}) {
		t.Fatalf("identity not ready: secret=%x err=%v", secret, err)
	}
	wantScope := filepath.Join(firstAuthDir, installationIdentityDir)
	activeInstallationIdentity.RLock()
	gotScope := activeInstallationIdentity.scope
	activeInstallationIdentity.RUnlock()
	if gotScope != wantScope {
		t.Fatalf("scope=%q want %q", gotScope, wantScope)
	}
	if err := configureInstallationIdentity(raw); err != nil {
		t.Fatalf("same scope reload failed: %v", err)
	}
	other := []byte(`{"host":{"auth_dir":"` + t.TempDir() + `"}}`)
	if err := configureInstallationIdentity(other); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("scope switch error = %v", err)
	}
}

func TestInstallationIdentityRejectsRelativeHostAuthDir(t *testing.T) {
	restoreInstallationIdentityFixture(t)
	clearInstallationSecretForTest()
	if err := configureInstallationIdentity([]byte(`{"host":{"auth_dir":"relative/auth"}}`)); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
	if _, err := currentInstallationSecret(); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("identity unexpectedly ready: %v", err)
	}
}

func TestInstallationIdentityHostScopeFailureLeavesIdentityUnready(t *testing.T) {
	restoreInstallationIdentityFixture(t)
	clearInstallationSecretForTest()
	authDir := t.TempDir()
	identityDir := filepath.Join(authDir, installationIdentityDir)
	if err := os.Mkdir(identityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(identityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"host":{"auth_dir":"` + authDir + `"}}`)
	if err := configureInstallationIdentity(raw); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
	if _, err := currentInstallationSecret(); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("identity unexpectedly ready: %v", err)
	}
}

func TestInstallationSecretRejectsSymlinkedAuthDirParent(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real-auth")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "auth-link")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateInstallationSecret(filepath.Join(alias, installationIdentityDir), bytes.NewReader(bytes.Repeat([]byte{1}, installationSecretBytes+8))); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallationSecretCreateReloadAndOfflineReset(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	firstEntropy := bytes.NewReader(bytes.Repeat([]byte{0x11}, installationSecretBytes+8))
	first, created, err := loadOrCreateInstallationSecret(dir, firstEntropy)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	if got := first[:]; !bytes.Equal(got, bytes.Repeat([]byte{0x11}, installationSecretBytes)) {
		t.Fatalf("unexpected key: %x", got)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("identity dir mode: info=%v err=%v", info, err)
	}
	keyPath := filepath.Join(dir, installationSecretFile)
	if info, err := os.Stat(keyPath); err != nil || info.Mode().Perm() != 0o600 || info.Size() != installationSecretBytes {
		t.Fatalf("identity key metadata: info=%v err=%v", info, err)
	}

	reloaded, created, err := loadOrCreateInstallationSecret(dir, failingEntropyReader{})
	if err != nil || created || reloaded != first {
		t.Fatalf("reload: created=%v same=%v err=%v", created, reloaded == first, err)
	}

	// Reset is intentionally offline-only: after the process releases its lock,
	// an operator may remove only installation.key and restart. The next start
	// creates a new namespace; auth credentials are not part of this file.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	secondEntropy := bytes.NewReader(bytes.Repeat([]byte{0x22}, installationSecretBytes+8))
	second, created, err := loadOrCreateInstallationSecret(dir, secondEntropy)
	if err != nil || !created || second == first {
		t.Fatalf("reset: created=%v changed=%v err=%v", created, second != first, err)
	}
}

func TestInstallationSecretRemovesSafeStaleTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, ".installation.key.tmp-deadbeef")
	if err := os.WriteFile(stale, bytes.Repeat([]byte{0x33}, installationSecretBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, created, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{0x44}, installationSecretBytes+8))); err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale temp was not removed: %v", err)
	}
}

func TestInstallationSecretRejectsWeakFilesAndSymlinks(t *testing.T) {
	for _, setup := range []struct {
		name string
		do   func(t *testing.T, dir string)
	}{
		{
			name: "world-readable",
			do: func(t *testing.T, dir string) {
				t.Helper()
				path := filepath.Join(dir, installationSecretFile)
				if err := os.WriteFile(path, bytes.Repeat([]byte{1}, installationSecretBytes), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "special-mode-bit",
			do: func(t *testing.T, dir string) {
				t.Helper()
				path := filepath.Join(dir, installationSecretFile)
				if err := os.WriteFile(path, bytes.Repeat([]byte{1}, installationSecretBytes), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o600|os.ModeSetuid); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt-length",
			do: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, installationSecretFile), bytes.Repeat([]byte{1}, installationSecretBytes-1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "multiple-hardlinks",
			do: func(t *testing.T, dir string) {
				t.Helper()
				key := filepath.Join(dir, installationSecretFile)
				if err := os.WriteFile(key, bytes.Repeat([]byte{1}, installationSecretBytes), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(key, filepath.Join(dir, "second-link")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			do: func(t *testing.T, dir string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, bytes.Repeat([]byte{1}, installationSecretBytes), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, installationSecretFile)); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(setup.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "identity")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			setup.do(t, dir)
			if _, _, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{2}, installationSecretBytes+8))); !errors.Is(err, errInstallationIdentity) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestInstallationSecretRejectsWeakLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, ".lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lockPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{2}, installationSecretBytes+8))); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallationSecretLockContentionFailsWithoutWaiting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dir, ".lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	if _, _, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{2}, installationSecretBytes+8))); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallationSecretRejectsWeakDirectoryMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{2}, installationSecretBytes+8))); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallationSecretCreationFailsWhenTempSuffixEntropyEnds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if _, _, err := loadOrCreateInstallationSecret(dir, bytes.NewReader(bytes.Repeat([]byte{7}, installationSecretBytes))); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, installationSecretFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key should not exist after suffix entropy failure: %v", err)
	}
}

func TestInstallationSecretCreationFailsWithoutEntropy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if _, _, err := loadOrCreateInstallationSecret(dir, io.LimitReader(failingEntropyReader{}, 1)); !errors.Is(err, errInstallationIdentity) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, installationSecretFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("key should not exist after entropy failure: %v", err)
	}
}
