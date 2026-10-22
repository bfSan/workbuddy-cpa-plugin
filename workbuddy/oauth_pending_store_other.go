//go:build !unix

package main

import "time"

// Non-Unix platforms intentionally remain memory-only until an equally secure
// directory/atomic-write/process-lock implementation exists. Start/poll report
// persistent=false; no fallback writes to HOME or the configured auth directory.
func openOAuthPendingStore(string) (*oauthPendingStore, error)      { return nil, nil }
func (*oauthPendingStore) read(string) (*oauthPendingRecord, error) { return nil, errOAuthStorage }
func (*oauthPendingStore) write(*oauthPendingRecord) error          { return errOAuthStorage }
func (*oauthPendingStore) scan(time.Time) (int, error)              { return 0, errOAuthStorage }
func (*oauthPendingStore) remove(string)                            {}
