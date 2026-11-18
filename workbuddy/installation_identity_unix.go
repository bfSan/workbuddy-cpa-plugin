//go:build unix

package main

import (
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const installationSecretFile = "installation.key"

func loadOrCreateInstallationSecret(dir string, entropy io.Reader) ([installationSecretBytes]byte, bool, error) {
	var zero [installationSecretBytes]byte
	if entropy == nil || !filepath.IsAbs(dir) {
		return zero, false, errInstallationIdentity
	}
	parentPath := filepath.Dir(dir)
	name := filepath.Base(dir)
	parentFD, err := unix.Open(parentPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return zero, false, errInstallationIdentity
	}
	defer unix.Close(parentFD)
	var stat unix.Stat_t
	if unix.Fstat(parentFD, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != uint32(os.Geteuid()) {
		return zero, false, errInstallationIdentity
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return zero, false, errInstallationIdentity
		}
	}

	dirFD, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return zero, false, errInstallationIdentity
	}
	defer unix.Close(dirFD)
	if unix.Fstat(dirFD, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 || stat.Uid != uint32(os.Geteuid()) {
		return zero, false, errInstallationIdentity
	}
	if unix.Fsync(parentFD) != nil {
		return zero, false, errInstallationIdentity
	}

	lockFD, err := unix.Openat(dirFD, ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return zero, false, errInstallationIdentity
	}
	defer unix.Close(lockFD)
	if unix.Fstat(lockFD, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return zero, false, errInstallationIdentity
	}
	if unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return zero, false, errInstallationIdentity
	}
	defer unix.Flock(lockFD, unix.LOCK_UN)
	if err := removeStaleInstallationTemps(dirFD); err != nil {
		return zero, false, errInstallationIdentity
	}

	if secret, err := readInstallationSecretAt(dirFD); err == nil {
		return secret, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return zero, false, errInstallationIdentity
	}

	var secret [installationSecretBytes]byte
	var suffix [8]byte
	if _, err := io.ReadFull(entropy, secret[:]); err != nil {
		return zero, false, errInstallationIdentity
	}
	if _, err := io.ReadFull(entropy, suffix[:]); err != nil {
		return zero, false, errInstallationIdentity
	}
	temp := ".installation.key.tmp-" + hex.EncodeToString(suffix[:])
	fd, err := unix.Openat(dirFD, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return zero, false, errInstallationIdentity
	}
	file := os.NewFile(uintptr(fd), "workbuddy-installation-secret")
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		_ = unix.Unlinkat(dirFD, temp, 0)
		return zero, false, errInstallationIdentity
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = unix.Unlinkat(dirFD, temp, 0)
		}
	}()
	if _, err := file.Write(secret[:]); err != nil || file.Sync() != nil || file.Close() != nil {
		return zero, false, errInstallationIdentity
	}
	if unix.Renameat(dirFD, temp, dirFD, installationSecretFile) != nil || unix.Fsync(dirFD) != nil {
		return zero, false, errInstallationIdentity
	}
	committed = true
	stored, err := readInstallationSecretAt(dirFD)
	if err != nil || stored != secret {
		return zero, false, errInstallationIdentity
	}
	return secret, true, nil
}

func removeStaleInstallationTemps(dirFD int) error {
	fd, err := unix.Openat(dirFD, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return errInstallationIdentity
	}
	dir := os.NewFile(uintptr(fd), "workbuddy-installation-identity-directory")
	defer dir.Close()
	entries, err := dir.ReadDir(130)
	if err != nil && err != io.EOF {
		return errInstallationIdentity
	}
	if len(entries) > 129 {
		return errInstallationIdentity
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".installation.key.tmp-") {
			continue
		}
		tempFD, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			return errInstallationIdentity
		}
		var stat unix.Stat_t
		valid := unix.Fstat(tempFD, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0o7777 == 0o600 && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid())
		_ = unix.Close(tempFD)
		if !valid || unix.Unlinkat(dirFD, name, 0) != nil {
			return errInstallationIdentity
		}
	}
	return nil
}

func readInstallationSecretAt(dirFD int) ([installationSecretBytes]byte, error) {
	var secret [installationSecretBytes]byte
	fd, err := unix.Openat(dirFD, installationSecretFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return secret, os.ErrNotExist
		}
		return secret, errInstallationIdentity
	}
	file := os.NewFile(uintptr(fd), "workbuddy-installation-secret")
	defer file.Close()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o7777 != 0o600 || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Size != installationSecretBytes {
		return secret, errInstallationIdentity
	}
	if _, err := io.ReadFull(file, secret[:]); err != nil {
		return secret, errInstallationIdentity
	}
	var extra [1]byte
	if count, err := file.Read(extra[:]); count != 0 || err != io.EOF {
		return [installationSecretBytes]byte{}, errInstallationIdentity
	}
	return secret, nil
}
