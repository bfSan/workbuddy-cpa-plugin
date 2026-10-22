package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCheckinRequiresAuthoritativeConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		pre                           *checkinSummary
		post                          *checkinSummary
		postErr                       error
		claimErr                      error
		receipt                       map[string]any
		want                          string
		confirmed, retry, confirmOnly bool
	}{
		{"verified", &checkinSummary{Active: true}, &checkinSummary{TodayCheckedIn: true}, nil, nil, map[string]any{"success": true}, "success", true, false, false},
		{"ack alone", &checkinSummary{Active: true}, &checkinSummary{Active: true}, nil, nil, map[string]any{"success": true}, "pending_confirmation", false, true, true},
		{"verify timeout", &checkinSummary{Active: true}, nil, &checkinRequestError{Kind: "transport", Retryable: true}, nil, map[string]any{"success": true}, "pending_confirmation", false, true, true},
		{"explicit rejection", &checkinSummary{Active: true}, &checkinSummary{Active: true}, nil, nil, map[string]any{"success": false, "message": "今日无法签到"}, "failed", false, false, false},
		{"already", &checkinSummary{TodayCheckedIn: true}, nil, nil, nil, nil, "already", true, false, false},
		{"no activity", &checkinSummary{Active: false}, nil, nil, nil, nil, "no_activity", false, false, false},
		{"post auth", &checkinSummary{Active: true}, nil, nil, &checkinRequestError{Kind: "auth"}, nil, "failed", false, false, false},
		{"post ambiguous", &checkinSummary{Active: true}, &checkinSummary{Active: true}, nil, &checkinRequestError{Kind: "transport", Retryable: true, Uncertain: true}, nil, "failed", false, true, true},
		{"post throttled", &checkinSummary{Active: true}, &checkinSummary{Active: true}, nil, &checkinRequestError{Kind: "rate_limit", Retryable: true}, nil, "failed", false, true, false},
		{"claimed but response lost", &checkinSummary{Active: true}, &checkinSummary{TodayCheckedIn: true}, nil, &checkinRequestError{Kind: "transport", Retryable: true, Uncertain: true}, nil, "success", true, false, false},
		{"already message unverified", &checkinSummary{Active: true}, &checkinSummary{Active: true}, nil, &checkinRequestError{Kind: "already"}, nil, "pending_confirmation", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, claims := 0, 0
			got := confirmedCheckinAttempt(checkinProtocol{
				status: func() (*checkinSummary, error) {
					reads++
					if reads == 1 {
						return tc.pre, nil
					}
					return tc.post, tc.postErr
				},
				claim: func() (map[string]any, error) { claims++; return tc.receipt, tc.claimErr },
			}, false)
			if got.State != tc.want || got.Confirmed != tc.confirmed || got.Retryable != tc.retry || got.ConfirmOnly != tc.confirmOnly {
				t.Fatalf("got=%+v", got)
			}
			if (tc.want == "already" || tc.want == "no_activity") && claims != 0 {
				t.Fatal("short-circuit emitted a claim")
			}
			if !got.Confirmed && got.Status != nil && got.Status.TodayCheckedIn {
				t.Fatal("fabricated checked-in cache")
			}
		})
	}
}

func TestCheckinStatusFailureNeverBlindlyClaims(t *testing.T) {
	for _, kind := range []string{"auth", "business", "protocol", "transport"} {
		t.Run(kind, func(t *testing.T) {
			got := confirmedCheckinAttempt(checkinProtocol{
				status: func() (*checkinSummary, error) {
					return nil, &checkinRequestError{Kind: kind, Retryable: kind == "transport"}
				},
				claim: func() (map[string]any, error) { t.Fatal("claim after failed status"); return nil, nil },
			}, false)
			if got.Confirmed || got.Retryable != (kind == "transport") {
				t.Fatalf("result=%+v", got)
			}
		})
	}
}

func TestCheckinConfirmationRetryDoesNotReplayPOST(t *testing.T) {
	got := confirmedCheckinAttempt(checkinProtocol{
		status: func() (*checkinSummary, error) { return &checkinSummary{Active: true}, nil },
		claim:  func() (map[string]any, error) { t.Fatal("confirmation must not POST again"); return nil, nil },
	}, true)
	if got.State != "pending_confirmation" || !got.Retryable {
		t.Fatalf("result=%+v", got)
	}
}

func TestCheckinRetryBudgetPerDayAndCancellation(t *testing.T) {
	s := &checkinRunStore{latest: make(map[string]checkinRunRecord)}
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))
	r := s.record("a", "id-a", "manual", checkinAttemptResult{State: "pending_confirmation", Retryable: true, ConfirmOnly: true}, now)
	if !s.needsConfirmation("a", now) {
		t.Fatal("manual repeats must remain verification-only")
	}
	for i, delay := range checkinRetryDelays {
		if !r.RetryAllowed || !r.NextRetryAt.Equal(now.Add(delay)) {
			t.Fatalf("retry %d: %+v", i, r)
		}
		if len(s.due(now, false)) != 0 {
			t.Fatal("retry fired early")
		}
		now = r.NextRetryAt
		if due := s.due(now, false); len(due) != 1 || due[0] != "a" {
			t.Fatalf("manual retries do not depend on auto toggle: %v", due)
		}
		r = s.record("a", "id-a", "retry", checkinAttemptResult{State: "pending_confirmation", Retryable: true, ConfirmOnly: true}, now)
	}
	if r.RetryAllowed || !r.NextRetryAt.IsZero() || r.RetryCount != 3 {
		t.Fatalf("unbounded retry: %+v", r)
	}
	// Another manual click may query now, but cannot reset today's retry budget.
	r = s.record("a", "id-a", "manual", checkinAttemptResult{State: "failed", Retryable: true}, now)
	if r.RetryAllowed {
		t.Fatal("manual click reset retry budget")
	}
	r = s.record("b", "id-b", "scheduled", checkinAttemptResult{State: "failed", Retryable: true}, now)
	_ = s.due(r.NextRetryAt, false)
	if _, due := s.retryMode("b", r.NextRetryAt); due {
		t.Fatal("auto disabled but retry remains due")
	}
}

func TestCheckinRetrySuccessNewDayAndBounds(t *testing.T) {
	s := &checkinRunStore{latest: make(map[string]checkinRunRecord)}
	now := time.Date(2026, 10, 22, 23, 59, 45, 0, time.Local)
	r := s.record("a", "id-a", "scheduled", checkinAttemptResult{State: "failed", Retryable: true}, now)
	if r.RetryAllowed {
		t.Fatal("retry crossed day boundary")
	}
	now = now.Add(10 * time.Minute)
	r = s.record("a", "id-a", "scheduled", checkinAttemptResult{State: "failed", Retryable: true}, now)
	if !r.RetryAllowed || r.RetryCount != 0 {
		t.Fatal("new day did not reset budget")
	}
	s.record("a", "id-a", "manual", checkinAttemptResult{State: "already", Confirmed: true}, now.Add(time.Second))
	if len(s.due(now.Add(time.Hour), true)) != 0 {
		t.Fatal("success left stale retry scheduled")
	}
	for i := 0; i < checkinLatestLimit+10; i++ {
		s.record(fmt.Sprint(i), "", "manual", checkinAttemptResult{State: "no_activity"}, now.Add(time.Duration(i)*time.Second))
	}
	latest, history := s.snapshot("")
	if len(latest) != checkinLatestLimit || len(history) != checkinHistoryLimit {
		t.Fatalf("unbounded records %d/%d", len(latest), len(history))
	}
	latest, history = s.snapshot("123")
	if len(latest) > 1 || len(history) > 1 {
		t.Fatal("filter leaks other accounts")
	}
}

func checkinTestHTTP(t *testing.T, status int, body string) {
	t.Helper()
	oldClient := sharedHTTPClient()
	oldRouting := proxyState.Load()
	sharedClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	proxyState.Store(&proxyRoutingState{mode: proxyModeInherit})
	t.Cleanup(func() { sharedClient = oldClient; proxyState.Store(oldRouting) })
}

func TestCheckinHTTPRejectsAmbiguousSuccessAndAuth(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body, kind string
		retry      bool
	}{
		{401, `{"code":0,"data":{"success":true}}`, "auth", false},
		{403, `{"code":0,"data":{"success":true}}`, "auth", false},
		{429, `{}`, "rate_limit", true},
		{503, `{}`, "server", true},
		{200, `{"data":{}}`, "protocol", false},
		{200, `{"code":0,"data":null}`, "protocol", false},
		{200, `{"code":112,"msg":"今日签到次数用尽 Bearer secret-token"}`, "business", false},
		{200, `{"code":113,"msg":"already checked in today"}`, "already", false},
	} {
		t.Run(fmt.Sprint(tc.status, tc.kind), func(t *testing.T) {
			checkinTestHTTP(t, tc.status, tc.body)
			_, err := checkinBillingCall(&storedAuth{}, "/v2/billing/meter/daily-checkin", "")
			ce, ok := err.(*checkinRequestError)
			if !ok || ce.Kind != tc.kind || ce.Retryable != tc.retry {
				t.Fatalf("err=%+v", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("secret leaked")
			}
		})
	}
}

func TestCheckinHTTPExplicitFalseNotRewritten(t *testing.T) {
	checkinTestHTTP(t, 200, `{"code":0,"data":{"success":false,"message":"今日不可领取"}}`)
	res, err := performCheckinCallWithCallback(&storedAuth{}, "")
	if err != nil || res["success"] != false {
		t.Fatalf("res=%v err=%v", res, err)
	}
}

func TestCheckinMalformedStatusDoesNotPermitClaim(t *testing.T) {
	for _, body := range []string{`{}`, `{"active":true}`, `{"active":"garbage","today_checked_in":false}`, `null`} {
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		if validatedCheckinStatus(m) == nil {
			t.Fatalf("accepted malformed status: %s", body)
		}
	}
	for _, body := range []string{`{"active":false}`, `{"today_checked_in":true}`, `{"active":true,"today_checked_in":false}`} {
		var m map[string]any
		_ = json.Unmarshal([]byte(body), &m)
		if validatedCheckinStatus(m) != nil {
			t.Fatalf("rejected valid status: %s", body)
		}
	}
}
