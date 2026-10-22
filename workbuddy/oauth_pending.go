package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	oauthPendingDir      = ".workbuddy-oauth-pending"
	oauthPendingMaxFiles = 128
	oauthPendingMaxBytes = 128 << 10
	oauthResultTTL       = 10 * time.Minute
)

// No URLs or transports are deserialized. Mode/region select a compiled-in
// profile; proxy policy is re-evaluated before every upstream request.
type oauthPendingRecord struct {
	Version int                  `json:"version"`
	State   string               `json:"state"`
	Mode    string               `json:"mode"`
	Region  string               `json:"region"`
	Created time.Time            `json:"created"`
	Expires time.Time            `json:"expires"`
	Cookies []oauthPendingCookie `json:"cookies,omitempty"`
	Token   *tokenData           `json:"token,omitempty"`
	TokenAt time.Time            `json:"token_at,omitempty"`
	Result  *storedAuth          `json:"result,omitempty"`
	Saved   bool                 `json:"saved,omitempty"`
}

type oauthPendingStore struct {
	dir  string
	fd   int      // pinned directory; openat/O_NOFOLLOW protects reads against symlinks
	lock *os.File // exclusive process ownership (not a cross-process queue)
}

var oauthPending = struct {
	sync.Mutex
	store   *oauthPendingStore
	initErr error
	recent  map[string]*loginCtx
}{recent: make(map[string]*loginCtx)}

var errOAuthStorage = errors.New("login persistence unavailable; retry without restarting the login")

func oauthPendingName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:]) + ".pending"
}

func validOAuthState(state string) bool {
	if len(state) == 0 || len(state) > 2048 || strings.TrimSpace(state) != state {
		return false
	}
	for _, c := range state {
		if c < 0x21 || c == 0x7f {
			return false
		}
	}
	return true
}

// configureOAuthPending accepts the Host summary on model/auth RPCs. Current
// hosts do NOT supply it on lifecycle RPCs. Empty means keep the established
// directory, or explicit memory-only mode if no directory was ever supplied.
// Errors are sticky for login start/poll, so model-discovery callers may ignore
// them without disabling models. A supplied unusable Unix directory never falls
// back to memory-only. No HOME/CWD fallback is used.
func configureOAuthPending(raw []byte) (err error) {
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
		return errors.New("invalid login host configuration")
	}
	dir := strings.TrimSpace(req.Host.AuthDir)
	if dir == "" {
		dir = strings.TrimSpace(req.Host.AuthDirSnake)
	}
	if dir == "" {
		return nil
	}
	oauthPending.Lock()
	defer oauthPending.Unlock()
	defer func() { oauthPending.initErr = err }()
	if !filepath.IsAbs(dir) {
		return errors.New("login auth directory must be absolute")
	}
	dir = filepath.Join(filepath.Clean(dir), oauthPendingDir)
	if oauthPending.store != nil {
		if oauthPending.store.dir == dir {
			return nil
		}
		// Rebinding existing tasks to another auth directory risks crossing host
		// identities. Require a process restart rather than silently migrating them.
		return errors.New("login auth directory changed; restart plugin to apply")
	}
	store, err := openOAuthPendingStore(dir)
	if err != nil {
		return err
	}
	oauthPending.store = store
	return nil
}

func oauthPendingStorageReady() error {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	return oauthPending.initErr
}

func validOAuthPendingName(name string) bool {
	if len(name) != 64+len(".pending") || !strings.HasSuffix(name, ".pending") {
		return false
	}
	_, err := hex.DecodeString(name[:64])
	return err == nil && strings.ToLower(name) == name
}

func validOAuthPendingRecord(r *oauthPendingRecord) bool {
	if r.Version != 1 || !validOAuthState(r.State) || (r.Mode != oauthClientModeCLI && r.Mode != oauthClientModeWorkBuddy) || (r.Region != oauthLoginRegionCN && r.Region != oauthLoginRegionGlobal) || (r.Mode == oauthClientModeCLI && r.Region != oauthLoginRegionCN) {
		return false
	}
	now := time.Now()
	if r.Created.IsZero() || r.Created.After(now.Add(time.Minute)) || !r.Expires.After(r.Created) || r.Expires.After(r.Created.Add(loginTTL+oauthResultTTL)) {
		return false
	}
	if r.Token != nil {
		if r.Token.AccessToken == "" || r.TokenAt.Before(r.Created) || r.TokenAt.After(now.Add(time.Minute)) || r.Expires.After(r.TokenAt.Add(oauthResultTTL)) {
			return false
		}
	} else if r.Result != nil || r.Saved || !r.TokenAt.IsZero() || r.Expires.After(r.Created.Add(loginTTL)) {
		return false
	}
	if r.Result != nil && (r.Result.Auth.AccessToken != r.Token.AccessToken || strings.TrimSpace(r.Result.Account.UID) == "") {
		return false
	}
	if r.Saved && r.Result == nil {
		return false
	}
	return validOAuthPendingCookies(r.Cookies, oauthProfileForModeRegion(r.Mode, r.Region))
}

func oauthRecord(state string, lc *loginCtx) *oauthPendingRecord {
	profile := lc.profile
	if profile.mode == "" {
		profile = oauthProfileForMode(oauthClientModeCLI)
	}
	region := oauthLoginRegionCN
	if profile.base == upstreamBaseGlobal {
		region = oauthLoginRegionGlobal
	}
	if lc.created.IsZero() {
		lc.created = time.Now()
	}
	rec := &oauthPendingRecord{Version: 1, State: state, Mode: profile.mode, Region: region, Created: lc.created, Expires: lc.expires, Token: lc.token, TokenAt: lc.tokenAt, Result: lc.result, Saved: lc.saved}
	if lc.client != nil {
		if jar, ok := lc.client.Jar.(*oauthCookieJar); ok {
			rec.Cookies = jar.snapshot()
		}
	}
	return rec
}

// Called with lc.mu held. Never drop in-memory stages after a disk error: a
// subsequent poll retries the checkpoint instead of consuming auth/token again.
func persistOAuthPending(state string, lc *loginCtx) error {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if lc.store == nil {
		return nil
	}
	return lc.store.write(oauthRecord(state, lc))
}

func registerOAuthPending(state string, lc *loginCtx) error {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.initErr != nil {
		return oauthPending.initErr
	}
	count := len(oauthPending.recent)
	loginStates.Range(func(_, _ any) bool { count++; return true })
	if count >= oauthPendingMaxFiles {
		return errors.New("too many pending logins; wait for expiry")
	}
	if _, ok := loginStates.Load(state); ok {
		return errors.New("duplicate login state; restart login")
	}
	if _, ok := oauthPending.recent[state]; ok {
		return errors.New("duplicate login state; restart login")
	}
	lc.store = oauthPending.store
	if lc.store != nil {
		if _, err := lc.store.read(oauthPendingName(state)); err == nil {
			return errors.New("duplicate login state; restart login")
		}
		if err := lc.store.write(oauthRecord(state, lc)); err != nil {
			return err
		}
	}
	loginStates.Store(state, lc)
	return nil
}

func lookupOAuthPending(state string) (*loginCtx, error) {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	if oauthPending.initErr != nil {
		return nil, oauthPending.initErr
	}
	if lc := oauthPending.recent[state]; lc != nil {
		return lc, nil
	}
	if v, ok := loginStates.Load(state); ok {
		return v.(*loginCtx), nil
	}
	if oauthPending.store != nil {
		rec, err := oauthPending.store.read(oauthPendingName(state))
		if err == nil && time.Now().Before(rec.Expires) {
			jar, err := restoreOAuthCookies(rec.Cookies)
			if err != nil {
				return nil, errOAuthStorage
			}
			// Restore without making requests, including while proxy config is blocked.
			client := &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: sharedHTTPClient().Transport, CheckRedirect: rejectHTTPRedirect}
			lc := &loginCtx{client: client, profile: oauthProfileForModeRegion(rec.Mode, rec.Region), created: rec.Created, expires: rec.Expires, token: rec.Token, tokenAt: rec.TokenAt, result: rec.Result, saved: rec.Saved, store: oauthPending.store, routingManaged: true}
			if rec.Token != nil {
				oauthPending.recent[state] = lc
			} else {
				loginStates.Store(state, lc)
			}
			return lc, nil
		}
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			oauthPending.store.remove(oauthPendingName(state))
		}
	}
	return nil, errors.New("poll: unknown state or expired login (restart login)")
}

func retainOAuthResult(state string, lc *loginCtx) {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	oauthPending.recent[state] = lc
	loginStates.Delete(state)
}

func removeOAuthPending(state string, lc *loginCtx) {
	oauthPending.Lock()
	defer oauthPending.Unlock()
	loginStates.CompareAndDelete(state, lc)
	if oauthPending.recent[state] == lc {
		delete(oauthPending.recent, state)
	}
	if lc.store != nil {
		lc.store.remove(oauthPendingName(state))
	}
}

func pruneOAuthPending(now time.Time) {
	type entry struct {
		state string
		lc    *loginCtx
	}
	var entries []entry
	oauthPending.Lock()
	loginStates.Range(func(k, v any) bool { entries = append(entries, entry{k.(string), v.(*loginCtx)}); return true })
	for state, lc := range oauthPending.recent {
		entries = append(entries, entry{state, lc})
	}
	oauthPending.Unlock()
	for _, e := range entries {
		if e.lc.mu.TryLock() {
			if !now.Before(e.lc.expires) {
				removeOAuthPending(e.state, e.lc)
			}
			e.lc.mu.Unlock()
		}
	}
	oauthPending.Lock()
	if oauthPending.store != nil {
		_, _ = oauthPending.store.scan(now)
	}
	oauthPending.Unlock()
}

func oauthLoginFailure(stage string, status int) error {
	// net/url.Error includes the entire URL (state); upstream messages can echo
	// tokens or auth codes. Never put either in API errors or host logs.
	if status > 0 {
		return fmt.Errorf("poll: %s (upstream HTTP %d); retry this login", stage, status)
	}
	return fmt.Errorf("poll: %s; retry this login", stage)
}
