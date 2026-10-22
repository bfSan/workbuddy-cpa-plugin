//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/sys/unix"
)

func isolateOAuthPending(t *testing.T, persistent bool) string {
	t.Helper()
	oldClient, oldProxy, oldSave := sharedHTTPClient(), currentProxyState(), hostAuthSaveJSONFn
	oldStates := map[string]*loginCtx{}
	oauthPending.Lock()
	loginStates.Range(func(k, v any) bool { oldStates[k.(string)] = v.(*loginCtx); loginStates.Delete(k); return true })
	oldStore, oldRecent, oldErr := oauthPending.store, oauthPending.recent, oauthPending.initErr
	oauthPending.initErr = nil
	oauthPending.store, oauthPending.recent = nil, make(map[string]*loginCtx)
	oauthPending.Unlock()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() {
		oauthPending.Lock()
		if s := oauthPending.store; s != nil {
			_ = s.lock.Close()
			_ = unix.Close(s.fd)
		}
		loginStates.Range(func(k, v any) bool { loginStates.Delete(k); return true })
		for k, v := range oldStates {
			loginStates.Store(k, v)
		}
		oauthPending.store, oauthPending.recent, oauthPending.initErr = oldStore, oldRecent, oldErr
		oauthPending.Unlock()
		sharedClient, hostAuthSaveJSONFn = oldClient, oldSave
		proxyState.Store(oldProxy)
	})
	if !persistent {
		return ""
	}
	dir := t.TempDir()
	if err := configureOAuthPending(mustJSON(map[string]any{"Host": map[string]string{"AuthDir": dir}})); err != nil {
		t.Fatal(err)
	}
	return dir
}

func restartOAuthPending(t *testing.T, dir string) {
	t.Helper()
	oauthPending.Lock()
	if s := oauthPending.store; s != nil {
		_ = s.lock.Close()
		_ = unix.Close(s.fd)
	}
	oauthPending.store, oauthPending.recent = nil, make(map[string]*loginCtx)
	loginStates.Range(func(k, v any) bool { loginStates.Delete(k); return true })
	oauthPending.Unlock()
	if err := configureOAuthPending(mustJSON(map[string]any{"Host": map[string]string{"AuthDir": dir}})); err != nil {
		t.Fatal(err)
	}
}

func startPendingForTest(region string) (pluginapi.AuthLoginStartResponse, error) {
	raw, err := startLoginWithModeRegion(oauthClientModeWorkBuddy, region)
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, err
	}
	var env struct {
		Result pluginapi.AuthLoginStartResponse
	}
	err = json.Unmarshal(raw, &env)
	return env.Result, err
}

func installPendingMock() (*atomic.Int32, *atomic.Int32) {
	starts, tokens := new(atomic.Int32), new(atomic.Int32)
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			state := fmt.Sprintf("state-%d", starts.Add(1))
			resp := testHTTPResponse(req, fmt.Sprintf(`{"code":0,"data":{"state":%q,"authUrl":"https://www.workbuddy.ai/login?state=%s&platform=workbuddy-ai"}}`, state, state))
			resp.Header.Set("Set-Cookie", "session="+state+"; Secure; HttpOnly; Path=/v2/plugin; Max-Age=1800")
			return resp, nil
		case "/v2/plugin/auth/token":
			tokens.Add(1)
			state := req.URL.Query().Get("state")
			if req.Header.Get("Cookie") != "session="+state {
				return nil, errors.New("cookie isolation failed")
			}
			domain := "codebuddy.cn"
			if req.URL.Host == "www.workbuddy.ai" {
				domain = "www.workbuddy.ai"
			}
			return testHTTPResponse(req, fmt.Sprintf(`{"code":0,"data":{"accessToken":%q,"refreshToken":"refresh","expiresIn":3600,"domain":%q}}`, "at-"+state, domain)), nil
		case "/v2/plugin/login/account":
			state := req.URL.Query().Get("state")
			if req.Header.Get("Authorization") != "Bearer at-"+state {
				return nil, errors.New("token isolation failed")
			}
			return testHTTPResponse(req, fmt.Sprintf(`{"code":0,"data":{"uid":%q,"nickname":"test"}}`, state)), nil
		}
		return nil, errors.New("unexpected path")
	})}
	return starts, tokens
}

func TestOAuthPendingConcurrentRestartAndIdempotentSave(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	_, tokens := installPendingMock()
	const count = 12
	starts := make([]pluginapi.AuthLoginStartResponse, count)
	var wg sync.WaitGroup
	for i := range starts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			region := oauthLoginRegionCN
			if i%2 != 0 {
				region = oauthLoginRegionGlobal
			}
			s, err := startPendingForTest(region)
			if err != nil {
				t.Error(err)
				return
			}
			starts[i] = s
		}(i)
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(dir, oauthPendingDir))
	if err != nil || len(entries) != count+1 {
		t.Fatalf("records=%d err=%v", len(entries), err)
	}
	info, _ := os.Stat(filepath.Join(dir, oauthPendingDir))
	if info.Mode().Perm() != 0700 {
		t.Fatal("directory permissions")
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		if info.Mode().Perm() != 0600 {
			t.Fatal("file permissions")
		}
		if entry.Name() != ".lock" && !validOAuthPendingName(entry.Name()) {
			t.Fatal("unhashed record")
		}
	}
	restartOAuthPending(t, dir)
	var saves atomic.Int32
	hostAuthSaveJSONFn = func(name string, raw []byte) error { saves.Add(1); return nil }
	for _, start := range starts {
		if start.Metadata["persistent"] != true {
			t.Fatal("persistence not advertised")
		}
		for range 4 {
			wg.Add(1)
			go func(state string) {
				defer wg.Done()
				result := handleOAuthPoll(pluginapi.ManagementRequest{Body: mustJSON(map[string]string{"state": state})})
				if result["success"] != true {
					t.Errorf("poll: %v", result)
				}
			}(start.State)
		}
	}
	wg.Wait()
	if tokens.Load() != count || saves.Load() != count {
		t.Fatalf("tokens=%d saves=%d", tokens.Load(), saves.Load())
	}
	restartOAuthPending(t, dir)
	// Completed-result recovery must be local, including under blocked routing.
	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	result := handleOAuthPoll(pluginapi.ManagementRequest{Body: mustJSON(map[string]string{"state": starts[0].State})})
	if result["success"] != true || tokens.Load() != count || saves.Load() != count {
		t.Fatalf("recovered result: %v", result)
	}
}

func TestOAuthPendingTokenCheckpointSurvivesAccountFailure(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	_, tokens := installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionGlobal)
	if err != nil {
		t.Fatal(err)
	}
	original := sharedClient.Transport
	var fail atomic.Bool
	fail.Store(true)
	sharedClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v2/plugin/login/account" && fail.Load() {
			r := testHTTPResponse(req, `{"code":500}`)
			r.StatusCode = 503
			return r, nil
		}
		return original.RoundTrip(req)
	})
	if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err == nil {
		t.Fatal("expected account failure")
	}
	rec, err := oauthPending.store.read(oauthPendingName(start.State))
	if err != nil || rec.Token == nil || rec.Result != nil {
		t.Fatalf("token checkpoint: %v", err)
	}
	tokenExpiry := rec.TokenAt.Add(time.Hour).Unix()
	restartOAuthPending(t, dir)
	fail.Store(false)
	raw, err := handlePollLogin(mustJSON(map[string]string{"state": start.State}))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Result pluginapi.AuthLoginPollResponse
	}
	_ = json.Unmarshal(raw, &env)
	sa, err := parseStored(env.Result.Auth.StorageJSON)
	if err != nil || sa.Auth.ExpiresAt != tokenExpiry || tokens.Load() != 1 {
		t.Fatalf("token was consumed again or expiry moved: %v calls=%d", err, tokens.Load())
	}
}

func TestOAuthPendingSaveFailureResultRetryAfterRestart(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	_, tokens := installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionCN)
	if err != nil {
		t.Fatal(err)
	}
	hostAuthSaveJSONFn = func(string, []byte) error { return errors.New("secret-token secret-code secret-state") }
	result := handleOAuthPoll(pluginapi.ManagementRequest{Body: mustJSON(map[string]string{"state": start.State})})
	if result["success"] != false || result["retryable"] != true || strings.Contains(fmt.Sprint(result), "secret-") {
		t.Fatalf("save failure: %v", result)
	}
	restartOAuthPending(t, dir)
	var saves atomic.Int32
	hostAuthSaveJSONFn = func(string, []byte) error { saves.Add(1); return nil }
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := handleOAuthPoll(pluginapi.ManagementRequest{Body: mustJSON(map[string]string{"state": start.State})})
			if r["success"] != true {
				t.Error(r)
			}
		}()
	}
	wg.Wait()
	if tokens.Load() != 1 || saves.Load() != 1 {
		t.Fatalf("tokens=%d saves=%d", tokens.Load(), saves.Load())
	}
}

func TestOAuthPendingCheckpointFailureDoesNotConsumeAgain(t *testing.T) {
	isolateOAuthPending(t, true)
	_, tokens := installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionCN)
	if err != nil {
		t.Fatal(err)
	}
	oauthPending.Lock()
	store := oauthPending.store
	fd := store.fd
	store.fd = -1
	oauthPending.Unlock()
	_, err = handlePollLogin(mustJSON(map[string]string{"state": start.State}))
	oauthPending.Lock()
	store.fd = fd
	oauthPending.Unlock()
	if err == nil {
		t.Fatal("disk failure reported success")
	}
	if _, err = handlePollLogin(mustJSON(map[string]string{"state": start.State})); err != nil {
		t.Fatal(err)
	}
	if tokens.Load() != 1 {
		t.Fatalf("token fetched %d times", tokens.Load())
	}
}

func TestOAuthPendingRejectsCorruptionExpiryAndURLInjection(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	installPendingMock()
	cases := []string{"corrupt", "expired", "url", "oversize", "symlink", "mismatch"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			start, err := startPendingForTest(oauthLoginRegionGlobal)
			if err != nil {
				t.Fatal(err)
			}
			name := oauthPendingName(start.State)
			path := filepath.Join(dir, oauthPendingDir, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			_ = json.Unmarshal(raw, &doc)
			switch kind {
			case "corrupt":
				raw = []byte("{")
			case "expired":
				doc["expires"] = time.Now().Add(-time.Second)
				raw = mustJSON(doc)
			case "url":
				doc["profile"] = map[string]string{"base": "http://127.0.0.1/private"}
				raw = mustJSON(doc)
			case "oversize":
				raw = []byte(strings.Repeat("x", oauthPendingMaxBytes+1))
			case "mismatch":
				doc["state"] = "other-state"
				raw = mustJSON(doc)
			case "symlink":
				outside := filepath.Join(dir, "outside")
				if err := os.WriteFile(outside, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "symlink" {
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			loginStates.Delete(start.State)
			if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err == nil {
				t.Fatal("unsafe record accepted")
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("unsafe record not pruned: %v", err)
			}
		})
	}
}

func TestOAuthPendingLimitsAndHashedTraversalState(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	state := "../../outside?state=a&injected=1"
	req, err := buildAuthTokenRequest(oauthProfileForMode(oauthClientModeCLI), state)
	if err != nil || req.URL.Query().Get("state") != state || len(req.URL.Query()) != 1 {
		t.Fatal("state query injection")
	}
	now := time.Now()
	lc := &loginCtx{created: now, expires: now.Add(loginTTL), profile: oauthProfileForMode(oauthClientModeCLI)}
	if err := registerOAuthPending(state, lc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oauthPendingDir, oauthPendingName(state))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < oauthPendingMaxFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, oauthPendingDir, fmt.Sprintf("unknown-%d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := registerOAuthPending("over-limit", &loginCtx{created: now, expires: now.Add(loginTTL), profile: oauthProfileForMode(oauthClientModeCLI)}); err == nil {
		t.Fatal("file count limit not enforced")
	}
}

func TestOAuthPendingMemoryModeRoutesAndNoCredentialLeak(t *testing.T) {
	isolateOAuthPending(t, false)
	installPendingMock()
	oldKey := getManagementAPIKeyForPendingTest()
	managementAPIKeyMu.Lock()
	managementAPIKey = ""
	managementAPIKeyMu.Unlock()
	t.Cleanup(func() { managementAPIKeyMu.Lock(); managementAPIKey = oldKey; managementAPIKeyMu.Unlock() })
	hostAuthSaveJSONFn = func(string, []byte) error { return nil }
	start, err := startPendingForTest(oauthLoginRegionCN)
	if err != nil {
		t.Fatal(err)
	}
	if start.Metadata["persistent"] != false {
		t.Fatal("memory-only mode claimed persistence")
	}
	raw, err := handleMethod(pluginabi.MethodManagementHandle, mustJSON(pluginapi.ManagementRequest{Method: http.MethodPost, Path: loadedManagementBasePath() + "/plugins/workbuddy/oauth/poll", Body: mustJSON(map[string]string{"state": start.State})}))
	if err != nil {
		t.Fatal(err)
	}
	var env struct{ Result pluginapi.ManagementResponse }
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env.Result.Body), "at-state") || strings.Contains(string(env.Result.Body), "refresh") {
		t.Fatal("panel returned credentials")
	}
	var result map[string]any
	if err := json.Unmarshal(env.Result.Body, &result); err != nil || result["success"] != true {
		t.Fatalf("route result %s %v", env.Result.Body, err)
	}
	// Host-provider path returns the same cached credential without another token call.
	if _, err := handleMethod(pluginabi.MethodAuthLoginPoll, mustJSON(map[string]string{"state": start.State})); err != nil {
		t.Fatal(err)
	}
}

func TestOAuthPendingInitFailureIsStickyAndRecoverable(t *testing.T) {
	isolateOAuthPending(t, false)
	starts, _ := installPendingMock()
	dir := t.TempDir()
	path := filepath.Join(dir, oauthPendingDir)
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	} // independent of the process umask
	raw := mustJSON(map[string]any{"Host": map[string]string{"AuthDir": dir}})
	if err := configureOAuthPending(raw); err == nil {
		t.Fatal("insecure directory accepted")
	}
	if _, err := startPendingForTest(oauthLoginRegionCN); err == nil || starts.Load() != 0 {
		t.Fatal("failed storage silently downgraded")
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := configureOAuthPending(raw); err != nil {
		t.Fatal(err)
	}
	if _, err := startPendingForTest(oauthLoginRegionCN); err != nil {
		t.Fatal(err)
	}
	// A second process/store cannot concurrently consume the same pending files.
	if second, err := openOAuthPendingStore(path); err == nil || second != nil {
		t.Fatal("store lock was not exclusive")
	}
}

func TestOAuthPendingExpiryPrunesResultAndDisk(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionCN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err != nil {
		t.Fatal(err)
	}
	pruneOAuthPending(time.Now().Add(loginTTL + oauthResultTTL + time.Minute))
	if _, err := os.Stat(filepath.Join(dir, oauthPendingDir, oauthPendingName(start.State))); !os.IsNotExist(err) {
		t.Fatal("expired result remains on disk")
	}
	if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err == nil {
		t.Fatal("expired result remains accessible")
	}
}

func TestOAuthPendingRecoveredRoutingUsesCurrentPolicy(t *testing.T) {
	dir := isolateOAuthPending(t, true)
	installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionGlobal)
	if err != nil {
		t.Fatal(err)
	}
	restartOAuthPending(t, dir)
	proxyState.Store(&proxyRoutingState{mode: proxyModeBlocked})
	if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err == nil {
		t.Fatal("blocked routing allowed restored request")
	}
	original := sharedClient.Transport
	var explicit atomic.Int32
	proxyState.Store(&proxyRoutingState{mode: proxyModeExplicit, client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		explicit.Add(1)
		if req.URL.Host != "www.workbuddy.ai" || req.Header.Get("Origin") != originRefererGlobal {
			return nil, errors.New("profile changed on restart")
		}
		// Removing an explicit proxy mid-poll must not reuse its old transport.
		if req.URL.Path == "/v2/plugin/auth/token" {
			proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
		}
		return original.RoundTrip(req)
	})}})
	if _, err := handlePollLogin(mustJSON(map[string]string{"state": start.State})); err != nil {
		t.Fatal(err)
	}
	if explicit.Load() != 1 {
		t.Fatalf("stale explicit transport reused %d times", explicit.Load())
	}
}

func TestOAuthPendingErrorsNeverEchoStateOrUpstreamSecrets(t *testing.T) {
	isolateOAuthPending(t, false)
	installPendingMock()
	start, err := startPendingForTest(oauthLoginRegionCN)
	if err != nil {
		t.Fatal(err)
	}
	sharedClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: req.URL.String() + "&code=secret-code", Err: errors.New("secret-token")}
	})
	_, err = handlePollLogin(mustJSON(map[string]string{"state": start.State}))
	if err == nil || strings.Contains(err.Error(), start.State) || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("secret error: %v", err)
	}
}

func getManagementAPIKeyForPendingTest() string {
	managementAPIKeyMu.RLock()
	defer managementAPIKeyMu.RUnlock()
	return managementAPIKey
}

func TestOAuthPendingRestoresCookieScopeAndExpiry(t *testing.T) {
	jar, err := newOAuthCookieJar()
	if err != nil {
		t.Fatal(err)
	}
	source, _ := url.Parse(upstreamBaseCN + "/v2/plugin/auth/state")
	jar.SetCookies(source, []*http.Cookie{{Name: "scoped", Value: "v", Secure: true, HttpOnly: true, Path: "/v2/plugin/auth", MaxAge: 60}, {Name: "gone", Value: "v", MaxAge: -1}})
	snapshot := jar.snapshot()
	restored, err := restoreOAuthCookies(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := url.Parse(upstreamBaseCN + "/v2/plugin/auth/token")
	account, _ := url.Parse(upstreamBaseCN + "/v2/plugin/login/account")
	if len(restored.Cookies(token)) != 1 || len(restored.Cookies(account)) != 0 || len(snapshot) != 1 || snapshot[0].Cookie.Expires.IsZero() || !snapshot[0].Cookie.HttpOnly {
		t.Fatal("cookie attributes not preserved")
	}
}
