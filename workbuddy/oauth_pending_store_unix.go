//go:build unix

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func openOAuthPendingStore(dir string) (*oauthPendingStore, error) {
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return nil, errOAuthStorage
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errOAuthStorage
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&0777 != 0700 || st.Uid != uint32(os.Geteuid()) {
		_ = unix.Close(fd)
		return nil, errOAuthStorage
	}
	lockFD, err := unix.Openat(fd, ".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		_ = unix.Close(fd)
		return nil, errOAuthStorage
	}
	lock := os.NewFile(uintptr(lockFD), "oauth-pending-lock")
	if unix.Fstat(lockFD, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0777 != 0600 || st.Nlink != 1 || st.Uid != uint32(os.Geteuid()) || unix.Flock(lockFD, unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = lock.Close()
		_ = unix.Close(fd)
		return nil, errOAuthStorage
	}
	store := &oauthPendingStore{dir: dir, fd: fd, lock: lock}
	if count, err := store.scan(time.Now()); err != nil || count > oauthPendingMaxFiles {
		_ = lock.Close()
		_ = unix.Close(fd)
		return nil, errOAuthStorage
	}
	return store, nil
}

func (s *oauthPendingStore) read(name string) (*oauthPendingRecord, error) {
	if !validOAuthPendingName(name) {
		return nil, errOAuthStorage
	}
	fd, err := unix.Openat(s.fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-record")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > oauthPendingMaxBytes {
		return nil, errOAuthStorage
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return nil, errOAuthStorage
	}
	raw, err := io.ReadAll(io.LimitReader(f, oauthPendingMaxBytes+1))
	if err != nil || len(raw) > oauthPendingMaxBytes {
		return nil, errOAuthStorage
	}
	var rec oauthPendingRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&rec) != nil || decoder.Decode(new(any)) != io.EOF || !validOAuthPendingRecord(&rec) || oauthPendingName(rec.State) != name {
		return nil, errOAuthStorage
	}
	return &rec, nil
}

// scan is bounded even if an external actor fills the directory. It removes
// invalid/expired records without logging their contents or filenames. Unknown
// files are counted, not deleted. Temporary files left by a crash are removed.
func (s *oauthPendingStore) scan(now time.Time) (int, error) {
	fd, err := unix.Openat(s.fd, ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, err
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-directory")
	defer f.Close()
	entries, err := f.ReadDir(oauthPendingMaxFiles*2 + 2)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if len(entries) > oauthPendingMaxFiles*2+1 {
		return 0, errOAuthStorage
	}
	count := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" {
			continue
		}
		if strings.HasPrefix(name, ".tmp-") && !entry.IsDir() {
			if unix.Unlinkat(s.fd, name, 0) != nil {
				return 0, errOAuthStorage
			}
			continue
		}
		if validOAuthPendingName(name) && !entry.IsDir() {
			rec, err := s.read(name)
			if err != nil || !now.Before(rec.Expires) {
				if unix.Unlinkat(s.fd, name, 0) != nil {
					return 0, errOAuthStorage
				}
				continue
			}
		}
		count++
	}
	return count, nil
}

func (s *oauthPendingStore) write(rec *oauthPendingRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil || len(raw) > oauthPendingMaxBytes || !validOAuthPendingRecord(rec) {
		return errOAuthStorage
	}
	count, err := s.scan(time.Now())
	if err != nil {
		return errOAuthStorage
	}
	name := oauthPendingName(rec.State)
	if _, err := s.read(name); err != nil && count >= oauthPendingMaxFiles {
		return errors.New("too many pending logins; wait for expiry")
	}
	// Create an unpredictable, exclusive, owner-only file in the pinned directory.
	temp := ".tmp-" + randomHex(16)
	fd, err := unix.Openat(s.fd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return errOAuthStorage
	}
	f := os.NewFile(uintptr(fd), "oauth-pending-temp")
	defer func() { _ = f.Close(); _ = unix.Unlinkat(s.fd, temp, 0) }()
	if _, err := f.Write(raw); err != nil {
		return errOAuthStorage
	}
	if f.Sync() != nil {
		return errOAuthStorage
	}
	if f.Close() != nil {
		return errOAuthStorage
	}
	if unix.Renameat(s.fd, temp, s.fd, name) != nil || unix.Fsync(s.fd) != nil {
		return errOAuthStorage
	}
	return nil
}

func (s *oauthPendingStore) remove(name string) {
	if validOAuthPendingName(name) {
		_ = unix.Unlinkat(s.fd, name, 0)
	}
}
