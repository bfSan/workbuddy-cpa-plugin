package main

import (
	"net/http"
	"testing"
)

// TestBillingHeadersMatchUpstreamContract pins the header set the billing
// backend requires. A bare Bearer request is rejected with 403
// {"code":10085,"msg":"请求不合法"} (verified live 2026-09-20), which made the
// panel render "积分未知" for every account.
func TestBillingHeadersMatchUpstreamContract(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", Domain: "copilot.tencent.com"},
		Account: storedAccount{UID: "uid-1"},
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.cn/v2/billing/meter/get-user-resource", nil)
	if err != nil {
		t.Fatal(err)
	}
	billingHeaders(req, sa)
	for _, key := range []string{
		"Authorization",
		"X-Requested-With",
		"X-CodeBuddy-Request",
		"User-Agent",
		"Origin",
		"Referer",
		"X-User-Id",
		"X-Domain",
		"X-Machine-ID",
		"X-Session-ID",
		"X-Request-ID",
		"X-No-Enterprise-Id",
	} {
		if req.Header.Get(key) == "" {
			t.Errorf("billing header %q is missing", key)
		}
	}
	if got := req.Header.Get("Origin"); got != "https://www.codebuddy.cn" {
		t.Errorf("CN origin = %q", got)
	}
}

func TestBillingHeadersUseGlobalOriginForGlobalAccounts(t *testing.T) {
	sa := &storedAuth{
		Auth:    storedTokens{AccessToken: "at", Domain: "www.workbuddy.ai"},
		Account: storedAccount{UID: "uid-2"},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/billing/meter/get-user-resource", nil)
	billingHeaders(req, sa)
	if got := req.Header.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Errorf("Global origin = %q", got)
	}
	if got := req.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("Global Accept-Language = %q", got)
	}
}

func TestDeriveBillingIDIsStablePerUID(t *testing.T) {
	a := deriveBillingID("uid-1", "machine")
	if a != deriveBillingID("uid-1", "machine") {
		t.Fatal("deriveBillingID must be deterministic")
	}
	if a == deriveBillingID("uid-2", "machine") {
		t.Fatal("different UIDs must not share an id")
	}
	if a == deriveBillingID("uid-1", "session") {
		t.Fatal("machine and session ids must differ")
	}
}
