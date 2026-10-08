package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestSplitLoginURLSeparatesBaseFromQuery covers the fix for the international
// sign-in that ended on "Login Failed".
//
// The host HTML-escapes every string in a plugin management response, so a URL
// reaches the browser with its separators written as the literal characters
// "&amp;". The browser reads "&amp;state=..." as one nameless parameter, the
// state never arrives, and the login page lands on /login/started with an empty
// state. The plugin cannot turn that escaping off, so it sends the URL in pieces
// that survive it: a base with no separator, plus the parameters as an object.
func TestSplitLoginURLSeparatesBaseFromQuery(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "international",
			raw:  "https://www.workbuddy.ai/login?platform=workbuddy-ai&state=abc&version=5.7.6",
			want: "https://www.workbuddy.ai/login",
		},
		{
			name: "china",
			raw:  "https://www.workbuddy.cn/login?platform=workbuddy&state=xyz&version=5.3.14",
			want: "https://www.workbuddy.cn/login",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, params := splitLoginURL(tc.raw)
			if base != tc.want {
				t.Errorf("base = %q, want %q", base, tc.want)
			}
			// The base must carry no separator at all: that is the whole point, and
			// a stray "?" or "&" would be escaped and reintroduce the bug.
			if strings.ContainsAny(base, "?&") {
				t.Errorf("base %q still contains a query separator", base)
			}
			if len(params) == 0 {
				t.Fatal("no parameters extracted")
			}
			for k, v := range params {
				// Parameter values must be free of the characters the host escapes,
				// otherwise the value itself would arrive corrupted.
				if strings.ContainsAny(v, "&<>\"'") || strings.ContainsAny(k, "&<>\"'") {
					t.Errorf("parameter %q=%q contains a character the host would escape", k, v)
				}
			}
		})
	}
}

// TestSplitLoginURLRoundTripsParameters proves the pieces rebuild the original
// query exactly, which is what makes the panel's URLSearchParams path correct.
func TestSplitLoginURLRoundTripsParameters(t *testing.T) {
	const raw = "https://www.workbuddy.ai/login?platform=workbuddy-ai&state=abc-def&version=5.7.6"
	base, params := splitLoginURL(raw)

	rebuilt, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	q := rebuilt.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	rebuilt.RawQuery = q.Encode()

	orig, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.Query().Encode() != orig.Query().Encode() {
		t.Errorf("round trip query = %q, want %q", rebuilt.Query().Encode(), orig.Query().Encode())
	}
	if got := rebuilt.Query().Get("state"); got != "abc-def" {
		t.Errorf("state = %q, want abc-def", got)
	}
}

// TestSplitLoginURLRejectsGarbage keeps the fallback honest: an unparseable URL
// returns an empty base so the panel uses its raw-string path instead of
// building a URL out of nothing.
func TestSplitLoginURLRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "   ", "not a url", "/login?state=x"} {
		base, params := splitLoginURL(bad)
		if base != "" || params != nil {
			t.Errorf("splitLoginURL(%q) = (%q, %v), want empty", bad, base, params)
		}
	}
}

// TestPanelRebuildsLoginURLFromParts pins the panel side. If someone reverts the
// panel to navigating d.url directly, the escaping bug returns silently.
func TestPanelRebuildsLoginURLFromParts(t *testing.T) {
	html := string(servePanel(""))
	if !strings.Contains(html, "function buildLoginURL(d)") {
		t.Fatal("panel lost buildLoginURL; it must rebuild the URL from url_base+query")
	}
	if !strings.Contains(html, "oauthURL=buildLoginURL(d)") {
		t.Error("panel must assign oauthURL through buildLoginURL, not d.url directly")
	}
	if strings.Contains(html, "oauthURL=d.url") {
		t.Error("panel assigns oauthURL=d.url directly, which reintroduces the escaped-URL bug")
	}
	if !strings.Contains(html, `replace(/&amp;/g,"&")`) {
		t.Error("panel fallback must undo the host's & escaping when url_base/query are absent")
	}
}

// TestOAuthStartReturnsURLParts checks the server actually sends the pieces the
// panel depends on.
func TestOAuthStartReturnsURLParts(t *testing.T) {
	oldFeatures := featureRuntime.Load()
	oldProxy := currentProxyState()
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	oldClient := sharedHTTPClient()
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return testHTTPResponse(req, `{"code":0,"data":{"state":"s1","authUrl":"https://www.workbuddy.ai/login?platform=workbuddy-ai&state=s1"}}`), nil
	})}
	t.Cleanup(func() {
		sharedClient = oldClient
		proxyState.Store(oldProxy)
		featureRuntime.Store(oldFeatures)
	})

	res := handleOAuthStart(pluginapi.ManagementRequest{Body: []byte(`{"region":"global"}`)})
	if ok, _ := res["success"].(bool); !ok {
		t.Fatalf("start failed: %+v", res)
	}
	base, _ := res["url_base"].(string)
	if base != "https://www.workbuddy.ai/login" {
		t.Errorf("url_base = %q, want the separator-free base", base)
	}
	params, ok := res["query"].(map[string]string)
	if !ok {
		t.Fatalf("query is %T, want map[string]string", res["query"])
	}
	if params["state"] != "s1" || params["platform"] != "workbuddy-ai" || params["version"] != "5.7.6" {
		t.Errorf("query = %v, want state/platform/version", params)
	}
	if got, _ := res["state"].(string); got != "s1" {
		t.Errorf("state = %q, want s1 so the panel polls the right flow", got)
	}
}
