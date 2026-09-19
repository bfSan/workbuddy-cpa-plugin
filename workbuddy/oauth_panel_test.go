package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleStartLogin reaches the upstream auth-state endpoint, so its outcome
// depends on the network a unit test must not assume. What matters here is
// that the panel endpoint never returns a half-built flow: either a complete
// URL plus state, or a failure that says why.
func TestHandleOAuthStart_NeverReturnsHalfBuiltFlow(t *testing.T) {
	res := handleOAuthStart()
	if ok, _ := res["success"].(bool); ok {
		if strings.TrimSpace(res["url"].(string)) == "" || strings.TrimSpace(res["state"].(string)) == "" {
			t.Fatalf("success must carry url and state, got %+v", res)
		}
		return
	}
	if msg, _ := res["error"].(string); strings.TrimSpace(msg) == "" {
		t.Fatalf("failure must carry a reason, got %+v", res)
	}
}

func TestHandleOAuthPoll_RequiresState(t *testing.T) {
	res := handleOAuthPoll(pluginapi.ManagementRequest{})
	if ok, _ := res["success"].(bool); ok {
		t.Fatalf("missing state should fail, got %+v", res)
	}
	if msg, _ := res["error"].(string); !strings.Contains(msg, "state") {
		t.Fatalf("error should name the missing field, got %q", msg)
	}
}

// The state token may arrive by query or body: the panel uses POST with a body,
// but a plain link/refresh path should work too.
func TestHandleOAuthPoll_AcceptsStateFromQueryOrBody(t *testing.T) {
	byQuery := handleOAuthPoll(pluginapi.ManagementRequest{
		Query: url.Values{"state": []string{"st-query"}},
	})
	// Unknown state fails (no such flow) rather than being silently accepted.
	if ok, _ := byQuery["success"].(bool); ok {
		t.Fatalf("unknown state should fail, got %+v", byQuery)
	}
	if msg, _ := byQuery["error"].(string); !strings.Contains(msg, "unknown state") {
		t.Fatalf("unknown state should say so, got %q", msg)
	}

	byBody := handleOAuthPoll(pluginapi.ManagementRequest{
		Body: mustMarshal(t, map[string]string{"state": "st-body"}),
	})
	if ok, _ := byBody["success"].(bool); ok {
		t.Fatalf("unknown state should fail, got %+v", byBody)
	}
}

func TestHandleOAuthPoll_RejectsBrokenBody(t *testing.T) {
	res := handleOAuthPoll(pluginapi.ManagementRequest{Body: []byte("{not json")})
	if ok, _ := res["success"].(bool); ok {
		t.Fatalf("broken body should fail, got %+v", res)
	}
	if msg, _ := res["error"].(string); !strings.Contains(msg, "state") {
		t.Fatalf("should fall through to the state check, got %q", msg)
	}
}

// The panel endpoints must not invent a second login state machine: they are
// registered alongside the existing RPC handlers and share loginStates.
func TestOAuthPanelRoutesAreRegistered(t *testing.T) {
	reg := managementRegistration()
	want := map[string]string{
		"/plugins/workbuddy/oauth/start": http.MethodPost,
		"/plugins/workbuddy/oauth/poll":  http.MethodPost,
	}
	found := map[string]bool{}
	for _, route := range reg.Routes {
		if method, ok := want[route.Path]; ok {
			if route.Method != method {
				t.Fatalf("route %s method = %s, want %s", route.Path, route.Method, method)
			}
			found[route.Path] = true
		}
	}
	for path := range want {
		if !found[path] {
			t.Fatalf("route %s is not registered", path)
		}
	}
}
