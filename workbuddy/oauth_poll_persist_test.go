package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// The panel drives login itself: handleOAuthStart opens the flow and
// handleOAuthPoll polls it. Neither goes through the host's auth.login.poll RPC,
// which is where the host would normally take the returned credential and write
// the auth file. These specs pin the consequence: a browser sign-in is only
// useful if the panel path persists it, and only the panel path can decide that.

// doJSONRequest unwraps the upstream {code,msg,data} envelope, so the stubs
// below have to carry it: a bare object parses as an empty data payload and the
// poll reads as "still pending" rather than as a completed login.
const (
	pollTokenJSON   = `{"code":0,"data":{"accessToken":"AT-PANEL-1","refreshToken":"RT-PANEL-1","expiresIn":3600,"domain":"www.workbuddy.ai"}}`
	pollAccountJSON = `{"code":0,"data":{"uid":"intl-uid-9","enterpriseId":"","nickname":"bfengsan"}}`
)

// stubPanelLogin registers an in-flight login whose client answers the two
// upstream calls a completed sign-in needs, and returns its state key.
func stubPanelLogin(t *testing.T, state string, tokenBody, accountBody string) {
	t.Helper()

	oldProxy := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { proxyState.Store(oldProxy) })

	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(req.URL.Path, "/v2/plugin/auth/token"):
			return testHTTPResponse(req, tokenBody), nil
		case strings.Contains(req.URL.Path, "/v2/plugin/login/account"):
			return testHTTPResponse(req, accountBody), nil
		}
		return testHTTPResponse(req, `{}`), nil
	})}

	loginStates.Store(state, &loginCtx{
		client:  client,
		expires: time.Now().Add(loginTTL),
		profile: oauthProfileForModeRegion(oauthClientModeWorkBuddy, oauthLoginRegionGlobal),
	})
	t.Cleanup(func() { loginStates.Delete(state) })
}

// stubAuthSave records what the panel path tried to persist.
func stubAuthSave(t *testing.T, err error) func() *string {
	t.Helper()

	orig := hostAuthSaveJSONFn
	var name string
	var body []byte
	hostAuthSaveJSONFn = func(n string, raw []byte) error {
		name, body = n, append([]byte(nil), raw...)
		return err
	}
	t.Cleanup(func() { hostAuthSaveJSONFn = orig })

	return func() *string {
		if name == "" {
			return nil
		}
		if len(body) == 0 {
			t.Helper()
			t.Errorf("saved %q with an empty document", name)
		}
		return &name
	}
}

func pollBody(state string) []byte {
	return []byte(`{"state":"` + state + `"}`)
}

// TestPanelPollPersistsCredentialOnSuccess is the regression for a sign-in that
// reported "登录成功，账号已写入 CPA" while no auth file was ever written.
//
// handlePollLogin only returns the credential. On the host RPC path that is
// correct, because the host writes the file from the returned AuthData. The panel
// calls handlePollLogin directly, and the old code read the nickname out of the
// payload and dropped the rest, so every panel login silently produced nothing.
func TestPanelPollPersistsCredentialOnSuccess(t *testing.T) {
	const state = "panel-poll-persist"
	stubPanelLogin(t, state, pollTokenJSON, pollAccountJSON)
	saved := stubAuthSave(t, nil)

	res := handleOAuthPoll(pluginapi.ManagementRequest{Body: pollBody(state)})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("poll should succeed, got %+v", res)
	}

	name := saved()
	if name == nil {
		t.Fatal("handleOAuthPoll never persisted the credential; the panel would claim success with no account on disk")
	}
	if *name != "workbuddy-intl-uid-9.json" {
		t.Errorf("saved as %q, want the uid-derived auth file name", *name)
	}
	if got, _ := res["nickname"].(string); got != "bfengsan" {
		t.Errorf("nickname = %q, want bfengsan", got)
	}
	// The login state must be consumed, or a second poll would re-save it.
	if _, still := loginStates.Load(state); still {
		t.Error("login state survived a successful poll and would be persisted twice")
	}
}

// TestPanelPollPendingPersistsNothing guards the opposite direction: while the
// browser has not finished, nothing may be written.
func TestPanelPollPendingPersistsNothing(t *testing.T) {
	const state = "panel-poll-pending"
	// Upstream answers 11217 "login ing..." with no token while pending.
	stubPanelLogin(t, state, `{"code":11217,"msg":"11217:login ing..."}`, pollAccountJSON)
	saved := stubAuthSave(t, nil)

	res := handleOAuthPoll(pluginapi.ManagementRequest{Body: pollBody(state)})
	if ok, _ := res["success"].(bool); ok {
		t.Fatalf("pending poll reported success: %+v", res)
	}
	if done, _ := res["done"].(bool); done {
		t.Error("pending poll reported done, which would stop the panel before the operator finishes")
	}
	if saved() != nil {
		t.Error("a pending login was persisted")
	}
}

// TestPanelPollSaveFailureIsNotReportedAsSuccess keeps a disk/host failure from
// wearing the success message. The browser sign-in did happen, so the error has
// to say that rather than blame the login.
func TestPanelPollSaveFailureIsNotReportedAsSuccess(t *testing.T) {
	const state = "panel-poll-savefail"
	stubPanelLogin(t, state, pollTokenJSON, pollAccountJSON)
	stubAuthSave(t, errForTest)

	res := handleOAuthPoll(pluginapi.ManagementRequest{Body: pollBody(state)})
	if ok, _ := res["success"].(bool); ok {
		t.Fatalf("a failed save reported success: %+v", res)
	}
	// done must still be true: polling again cannot fix a save failure.
	if done, _ := res["done"].(bool); !done {
		t.Error("save failure left done=false, so the panel would poll a finished login until the deadline")
	}
	msg, _ := res["error"].(string)
	if msg == "" {
		t.Fatal("save failure returned no error for the panel to show")
	}
	if !strings.Contains(msg, "succeed") || !strings.Contains(msg, "saving") {
		t.Errorf("error = %q, want it to say the sign-in worked but saving did not", msg)
	}
}

var errForTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "host.auth.save: simulated" }

// TestPanelClosesOAuthModalOnSuccess covers the other half of the report: the
// dialog stayed open after a completed login, sitting over the account list the
// operator needs next.
func TestPanelClosesOAuthModalOnSuccess(t *testing.T) {
	html := string(servePanel(""))

	at := strings.Index(html, "if(d.success){")
	if at < 0 {
		t.Fatal("panel no longer has the poll success branch")
	}
	// The branch carries explanatory comments, so the window has to clear them.
	block := html[at:]
	if end := strings.Index(block, "if(d.done){"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "closeOAuthModal()") {
		t.Error("the login success branch does not close the modal")
	}
	if strings.Contains(block, "stopOAuthPolling()") && !strings.Contains(block, "closeOAuthModal()") {
		t.Error("the success branch only stops polling; the dialog stays open")
	}

	// A terminal failure must surface the server's reason. handleOAuthPoll now
	// sends "error" (for example "login succeeded but saving the account
	// failed"), and reading only d.message would hide it.
	if !strings.Contains(html, "d.error||d.message") {
		t.Error("the poll failure branch ignores d.error, hiding why a completed login was not saved")
	}
}
