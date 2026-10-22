package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quota6004Sample = `{"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"private-request-id"}`
const quotaDailySample = `{"code":50001,"msg":"今日免费模型请求次数已达上限"}`

func TestQuotaClassificationNarrowEvidence(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		reason     cooldownReason
		scope      string
		credential bool
	}{
		{"WorkBuddy 6004", 429, quota6004Sample, cooldownReasonRateLimit, quotaScopeModel, false},
		{"stream 6004", 200, quota6004Sample, cooldownReasonRateLimit, quotaScopeModel, false},
		{"temporary", 429, `{"msg":"too many requests"}`, cooldownReasonRateLimit, quotaScopeModel, false},
		{"daily 403", 403, quotaDailySample, cooldownReasonCallQuota, quotaScopeModel, false},
		{"daily 401", 401, `{"msg":"daily usage allowance exhausted"}`, cooldownReasonCallQuota, quotaScopeModel, false},
		{"daily split", 200, `{"error":{"message":"每日请求额度已用尽，明天再来"}}`, cooldownReasonCallQuota, quotaScopeModel, false},
		{"generic quota never account", 403, `{"scope":"account","msg":"quota exceeded"}`, cooldownReasonCallQuota, quotaScopeModel, false},
		{"insufficient credits scope unknown", 403, `{"msg":"insufficient credits"}`, cooldownReasonCreditQuota, quotaScopeModel, false},
		{"shared insufficient not depleted", 403, `{"msg":"insufficient shared credits"}`, cooldownReasonCreditQuota, quotaScopeModel, false},
		{"account insufficient not depleted", 403, `{"scope":"account","msg":"insufficient credits"}`, cooldownReasonCreditQuota, quotaScopeModel, false},
		{"shared exhausted", 403, `{"msg":"shared credits exhausted"}`, cooldownReasonSharedCredits, quotaScopeAccount, false},
		{"shared zh", 402, `{"msg":"共享积分已耗尽"}`, cooldownReasonSharedCredits, quotaScopeAccount, false},
		{"explicit account credit scope", 402, `{"error":{"scope":"account","message":"credits exhausted"}}`, cooldownReasonSharedCredits, quotaScopeAccount, false},
		{"unknown 402", 402, "", "", "", false},
		{"unknown code110", 403, `{"code":110,"msg":"unknown rejection"}`, "", "", false},
		{"unknown quota", 403, `{"msg":"quota unavailable"}`, "", "", false},
		{"quota informational", 200, `{"quota":0,"message":"quota status unknown"}`, "", "", false},
		{"not exhausted", 403, `{"msg":"shared credits not exhausted"}`, "", "", false},
		{"daily unrelated", 400, `{"msg":"daily digest: the sky is the limit"}`, "", "", false},
		{"invalid JSON", 403, `{"msg":"quota exceeded"`, "", "", false},
		{"echoed request", 403, `{"request":{"message":"shared credits exhausted"},"msg":"rejected"}`, "", "", false},
		{"model unavailable", 400, `{"code":11102,"msg":"service info not found"}`, "", "", false},
		{"prompt limit", 400, `{"code":11115,"msg":"daily context length exceeded"}`, "", "", false},
		{"session revoked", 400, `{"code":12153,"msg":"Offline user session not found"}`, "", quotaScopeAccount, true},
		{"token invalid", 401, `{"message":"invalid token"}`, "", quotaScopeAccount, true},
		{"choices are content", 200, `{"choices":[{"delta":{"content":"shared credits exhausted"}}]}`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyChatFailure(tc.status, tc.body)
			if got.Reason != tc.reason || got.Scope != tc.scope || got.CredentialInvalid != tc.credential {
				t.Fatalf("classification = %+v, want reason=%s scope=%s credential=%v", got, tc.reason, tc.scope, tc.credential)
			}
		})
	}
}

func TestQuotaIsolationExpiryAndClear(t *testing.T) {
	resetCooldowns(t)
	now := time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC)
	cooldownNowFn = func() time.Time { return now }
	recordUpstreamFailure("a", "m1", 403, quotaDailySample)
	if guardExecutorQuota("a", "m1") == nil || guardExecutorQuota("a", "m2") != nil || guardExecutorQuota("b", "m1") != nil {
		t.Fatal("pair isolation failed")
	}
	entry, _ := quotaFreezeFor("a", "m1")
	if entry.Until != now.Add(quotaProbeTTL) || entry.RecoveryCondition != "bounded_ttl_probe" {
		t.Fatalf("must probe bounded TTL, not guess midnight: %+v", entry)
	}
	start := entry.OccurredAt
	now = now.Add(time.Minute)
	markModelCooldown("a", "m1", cooldownReasonUnknown)
	entry, _ = quotaFreezeFor("a", "m1")
	if entry.Reason != cooldownReasonCallQuota || entry.OccurredAt != start {
		t.Fatalf("weaker signal replaced hold: %+v", entry)
	}
	now = entry.Until
	if guardExecutorQuota("a", "m1") != nil {
		t.Fatal("at next_probe_at the pair must be eligible to probe")
	}

	recordUpstreamFailure("a", "m1", 403, quotaDailySample)
	recordUpstreamFailure("a", "m2", 402, `{"msg":"shared credits exhausted"}`)
	if guardExecutorQuota("a", "any-model") == nil || guardExecutorQuota("b", "any-model") != nil {
		t.Fatal("shared scope isolation failed")
	}
	if clearModelCooldown("a", "m1") != 1 || guardExecutorQuota("a", "m1") == nil {
		t.Fatal("pair clear must not clear a shared hold")
	}
	if clearModelCooldown("a", "") != 1 || guardExecutorQuota("a", "m1") != nil {
		t.Fatal("explicit account clear must clear the shared hold")
	}
}

func TestQuotaNoModelCannotBroadenUnprovenScope(t *testing.T) {
	resetCooldowns(t)
	for _, body := range []string{quotaDailySample, `{"msg":"insufficient credits"}`, `{"msg":"rate limit"}`} {
		recordUpstreamFailure("a", "", 403, body)
	}
	if rows := cooldownSnapshotAll(); len(rows) != 0 {
		t.Fatalf("missing model broadened scope: %+v", rows)
	}
	recordUpstreamFailure("a", "", 402, `{"msg":"shared credits exhausted"}`)
	if guardExecutorQuota("a", "m") == nil {
		t.Fatal("explicit shared credit evidence must not require a model")
	}
}

func TestQuotaKnownRetryAndResetOnly(t *testing.T) {
	now := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		h        http.Header
		body     string
		delay    time.Duration
		recovery string
	}{
		{"seconds", http.Header{"Retry-After": {"17"}}, "", 17 * time.Second, "upstream_retry_after"},
		{"date", http.Header{"Retry-After": {now.Add(time.Hour).Format(http.TimeFormat)}}, "", time.Hour, "upstream_retry_after"},
		{"milliseconds", http.Header{"Retry-After-Ms": {"1500"}}, "", 1500 * time.Millisecond, "upstream_retry_after"},
		{"copy", nil, quota6004Sample, 46*time.Minute + 39*time.Second, "upstream_reset"},
		{"no timezone no assumption", nil, `{"msg":"daily limit reached，将在 2026-09-22 10:00:00 重置"}`, 0, "bounded_ttl_probe"},
		{"negative", http.Header{"Retry-After": {"-2"}}, "", 0, "bounded_ttl_probe"},
		{"overflow", http.Header{"Retry-After": {"9223372036854775807"}}, "", 0, "bounded_ttl_probe"},
		{"huge", http.Header{"Retry-After": {"86401"}}, "", 0, "bounded_ttl_probe"},
		{"ambiguous reset unit", http.Header{"X-Ratelimit-Reset": {"60"}}, "", 0, "bounded_ttl_probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at, reason := quotaRetryAt(tc.h, tc.body, now)
			if reason != tc.recovery || (tc.delay == 0 && !at.IsZero()) || (tc.delay > 0 && !at.Equal(now.Add(tc.delay))) {
				t.Fatalf("got %v %s", at, reason)
			}
		})
	}
}

func TestQuotaReportsOnlySafeSummariesAndClearNeverEnables(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("a", "m", 403, `{"msg":"daily limit reached Bearer top-secret","requestId":"secret-request"}`)
	res := handleCooldownList(pluginapi.ManagementRequest{Query: url.Values{"auth_id": {"a"}}})
	raw, _ := json.Marshal(res)
	for _, secret := range []string{"top-secret", "secret-request", "Bearer"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("report leaked upstream text")
		}
	}
	rows := res["entries"].([]map[string]any)
	for _, key := range []string{"reason", "scope", "occurred_at", "until", "next_probe_at", "recovery_condition", "evidence", "storage"} {
		if rows[0][key] == nil || rows[0][key] == "" {
			t.Fatalf("missing explanation %s", key)
		}
	}
	if res["persistent"] != false {
		t.Fatal("process-local limitation must be public")
	}
	manual := []byte(`{"disabled":true,"disabled_reason":"manual"}`)
	handleCooldownClear(pluginapi.ManagementRequest{Body: []byte(`{"auth_id":"a"}`)})
	if !parseDisabledFromAuthJSON(manual) || shouldReenableCN(true, &creditsSummary{TotalRemain: 9}, authFileDisabledReason(manual), "") {
		t.Fatal("freeze clear must never authorize manual enable")
	}
	if shouldReenableCN(true, &creditsSummary{TotalRemain: 9}, "", "") {
		t.Fatal("unknown disable provenance cannot be auto-enabled")
	}
}

func TestQuotaFreshCreditsOnlyClearSharedHold(t *testing.T) {
	resetCooldowns(t)
	recordUpstreamFailure("a", "m", 403, quotaDailySample)
	recordUpstreamFailure("a", "m", 402, `{"msg":"shared credits exhausted"}`)
	observeQuotaCredits("a", nil)
	observeQuotaCredits("a", &creditsSummary{TotalRemain: 0, TotalUsed: 10})
	if guardExecutorQuota("a", "other") == nil {
		t.Fatal("unknown/zero balance must not clear the hold")
	}
	observeQuotaCredits("a", &creditsSummary{TotalRemain: 1})
	if guardExecutorQuota("a", "other") != nil || guardExecutorQuota("a", "m") == nil {
		t.Fatal("credit recovery must not clear model allowance")
	}
	observeQuotaCredits("b", &creditsSummary{TotalRemain: 0, TotalUsed: 10})
	if guardExecutorQuota("b", "free") != nil {
		t.Fatal("zero balance alone must leave free models available")
	}
}

func TestQuotaErrorPackagingAvoidsCredentialStatus(t *testing.T) {
	for _, status := range []int{200, 400, 401, 402, 403, 429} {
		err := chatUpstreamError(status, quotaDailySample)
		if err.StatusCode() != 0 || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("unsafe quota wrapper: %+v", err)
		}
		for _, code := range []string{"upstream 401", "upstream 402", "upstream 403", "upstream 429"} {
			if strings.Contains(err.Error(), code) {
				t.Fatalf("raw status leaked: %s", err)
			}
		}
		var env envelope
		_ = json.Unmarshal(errorEnvelopeFor(err), &env)
		if env.Error == nil || env.Error.HTTPStatus != 0 {
			t.Fatalf("RPC reintroduced credential status: %+v", env)
		}
	}
	for _, status := range []int{401, 402, 403} {
		err := chatUpstreamError(status, `{"msg":"quota unavailable"}`)
		if err.StatusCode() != 0 {
			t.Fatalf("ambiguous quota treated as auth failure: %+v", err)
		}
	}
	if err := chatUpstreamError(403, `{"code":12153,"msg":"Offline user session not found"}`); err.StatusCode() != 401 {
		t.Fatalf("credential invalidation lost: %+v", err)
	}
}

func TestQuotaStreamAndFoldedBoundaries(t *testing.T) {
	for _, prefix := range []string{"", "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"} {
		t.Run(map[bool]string{true: "before-first", false: "after-first"}[prefix == ""], func(t *testing.T) {
			resetCooldowns(t)
			wire := prefix + "data: " + quotaDailySample + "\n\ndata: [DONE]\n\n"
			_, err := aggregateCompletionWithQuota(strings.NewReader(wire), "alias", "a", "upstream-m", nil)
			if err == nil || !strings.Contains(err.Error(), "call_quota") || !modelIsCooling("a", "upstream-m") || modelIsCooling("a", "alias") {
				t.Fatalf("folded business frame boundary failed: %v", err)
			}
			clearModelCooldown("a", "")
			_, err = aggregateSSEObserved(strings.NewReader(wire), true, nil, func(body string) { recordUpstreamFailure("a", "upstream-m", 200, body) })
			if err == nil || !modelIsCooling("a", "upstream-m") {
				t.Fatalf("stream boundary failed: %v", err)
			}
		})
	}
	resetCooldowns(t)
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"daily limit reached\"}}]}\n\ndata: [DONE]\n\n"
	if _, err := aggregateCompletionWithQuota(strings.NewReader(wire), "m", "a", "m", nil); err != nil || modelIsCooling("a", "m") {
		t.Fatalf("normal content mistaken for quota: %v", err)
	}
	if err := chatStreamFrameError(`{"code":110,"msg":"unknown error"}`); err == nil || err.StatusCode() != 0 {
		t.Fatal("unknown error envelope must fail without inventing quota/auth")
	}
}

func quotaHTTPClient(t *testing.T, status int, body string, calls *int) {
	t.Helper()
	old := sharedHTTPClient()
	sharedClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		*calls++
		return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"37"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}
	t.Cleanup(func() { sharedClient = old })
}

func TestQuotaQueuedDispatchRechecksWithoutNetwork(t *testing.T) {
	resetCooldowns(t)
	calls := 0
	quotaHTTPClient(t, 200, "", &calls)
	hostCalls := recordStreamHostCalls(t, nil)
	queued := pumpTestRequest(t)
	// The request was constructed/queued before the other request froze its pair.
	recordUpstreamFailure("a", "m", 403, quotaDailySample)
	pumpUpstreamStream(queued, nil, "s", false, "m", "m", "", time.Now(), "a", "")
	if calls != 0 || streamCloseCallCount(*hostCalls) != 1 || len(nativeStreamErrors(t, *hostCalls)) != 1 {
		t.Fatal("queued async request escaped the freeze")
	}
	_, _, err := collectUpstreamStream([]byte(`{}`), &storedAuth{}, false, nil, "", "a", "m")
	if err == nil || calls != 0 {
		t.Fatal("sync collector escaped the freeze")
	}
}

func TestQuotaStreamHTTPAndInBandRecordBeforeEmit(t *testing.T) {
	for _, status := range []int{403, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetCooldowns(t)
			calls := 0
			body := quotaDailySample
			if status == 200 {
				body = "data: " + body + "\n\n"
			}
			quotaHTTPClient(t, status, body, &calls)
			hostCalls := recordStreamHostCalls(t, nil)
			pumpUpstreamStream(pumpTestRequest(t), nil, "s", false, "m", "m", "", time.Now(), "a", "")
			if calls != 1 || !modelIsCooling("a", "m") || modelIsCooling("a", "other") {
				t.Fatal("stream failure scope missing")
			}
			messages := nativeStreamErrors(t, *hostCalls)
			if len(messages) != 1 || strings.Contains(messages[0], "403") || !strings.Contains(messages[0], "unexpected EOF") {
				t.Fatalf("unsafe stream error: %v", messages)
			}
			entry, _ := quotaFreezeFor("a", "m")
			if entry.RecoveryCondition != "upstream_retry_after" {
				t.Fatal("upstream retry headers discarded")
			}
		})
	}
}

func TestQuotaManualDisableWinsOverStaleReenableSnapshot(t *testing.T) {
	oldGet, oldPersist := lifecycleHostAuthGetPhysical, hostAuthPersistFn
	t.Cleanup(func() { lifecycleHostAuthGetPhysical, hostAuthPersistFn = oldGet, oldPersist })
	saves := 0
	hostAuthPersistFn = func(name, path string, raw []byte) error { saves++; return nil }
	for _, reason := range []string{disableReasonManual, "", "operator_lock"} {
		raw, _ := json.Marshal(map[string]any{"disabled": true, "disabled_reason": reason, "note": "CN · 已禁用 · 耗尽"})
		lifecycleHostAuthGetPhysical = func(string) (*hostAuthPhysical, error) { return &hostAuthPhysical{JSON: raw, Disabled: true}, nil }
		// Simulate a caller holding old exhausted metadata while the live file
		// was manually disabled during its billing query.
		if err := reenableAuth("quota-manual", "quota-manual", &storedAuth{}, &creditsSummary{TotalRemain: 9}, disableReasonExhausted, ""); err != nil {
			t.Fatal(err)
		}
		if err := disableAuth("quota-manual", "quota-manual", &storedAuth{}, nil, "exhausted", disableReasonExhausted); err != nil {
			t.Fatal(err)
		}
	}
	if saves != 0 {
		t.Fatalf("stale automation rewrote manual state %d times", saves)
	}
}

func TestQuotaExecutorHTTPGuardsOnlyExactChat(t *testing.T) {
	resetCooldowns(t)
	const id = "quota-generic-http"
	installModelStatesForTest(t, map[string]modelReadinessState{id: modelReady})
	calls := 0
	quotaHTTPClient(t, 200, `{}`, &calls)
	sa := &storedAuth{Auth: storedTokens{AccessToken: "test-only"}}
	request := pluginapi.ExecutorHTTPRequest{AuthID: id, Method: http.MethodPost, URL: endpointChatFor(sa), StorageJSON: mustJSON(sa), Body: []byte(`{"model":"m"}`)}
	invoke := func(req pluginapi.ExecutorHTTPRequest) envelope {
		t.Helper()
		raw, err := handleExecHTTPRequest(mustJSON(req))
		if err != nil {
			t.Fatal(err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
		return env
	}
	recordUpstreamFailure(id, "m", 403, quotaDailySample)
	if env := invoke(request); env.OK || env.Error == nil || env.Error.Code != "workbuddy_quota_hold" || calls != 0 {
		t.Fatalf("chat bypassed pair guard: %+v calls=%d", env, calls)
	}
	// Unknown model has no invented association with m: only shared holds apply.
	request.Body = []byte(`{"model":27}`)
	if env := invoke(request); !env.OK || calls != 1 {
		t.Fatalf("unknown model incorrectly inherited pair state: %+v", env)
	}
	recordUpstreamFailure(id, "m", 402, `{"msg":"shared credits exhausted"}`)
	if env := invoke(request); env.OK || calls != 1 {
		t.Fatalf("unknown model bypassed shared hold: %+v", env)
	}
	request.URL = upstreamBaseFor(sa) + "/v2/billing/meter/get-user-resource"
	if env := invoke(request); !env.OK || calls != 2 {
		t.Fatalf("billing blocked by chat quota: %+v", env)
	}
	request.URL = endpointChatFor(sa) + "/different"
	if env := invoke(request); !env.OK || calls != 3 {
		t.Fatalf("unrelated endpoint blocked: %+v", env)
	}
}

func TestQuotaExecutorHTTPRecordsBusinessErrors(t *testing.T) {
	for _, status := range []int{401, 403, 200} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			resetCooldowns(t)
			const id = "quota-generic-error"
			installModelStatesForTest(t, map[string]modelReadinessState{id: modelReady})
			calls := 0
			body := quotaDailySample
			if status == 200 {
				body = "data: " + body + "\n\n"
			}
			quotaHTTPClient(t, status, body, &calls)
			sa := &storedAuth{Auth: storedTokens{AccessToken: "test-only"}}
			request := pluginapi.ExecutorHTTPRequest{AuthID: id, Method: http.MethodPost, URL: endpointChatFor(sa), StorageJSON: mustJSON(sa), Body: []byte(`{"model":"m"}`)}
			raw, err := handleExecHTTPRequest(mustJSON(request))
			if err != nil {
				t.Fatal(err)
			}
			var env envelope
			_ = json.Unmarshal(raw, &env)
			if env.OK || env.Error == nil || env.Error.Code != "workbuddy_quota_hold" || env.Error.HTTPStatus != 0 || !modelIsCooling(id, "m") || modelIsCooling(id, "other") {
				t.Fatalf("unsafe HTTP chat failure: %s", raw)
			}
		})
	}
}

func TestQuotaMainExecutorBoundariesAndCacheGuard(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, status := range []int{401, 403, 200} {
			t.Run(map[bool]string{true: "stream", false: "folded"}[stream]+http.StatusText(status), func(t *testing.T) {
				resetCooldowns(t)
				const id = "quota-main-error"
				installModelStatesForTest(t, map[string]modelReadinessState{id: modelReady})
				calls := 0
				body := quotaDailySample
				if status == 200 {
					body = "data: " + body + "\n\n"
				}
				quotaHTTPClient(t, status, body, &calls)
				request := executorTestRequest()
				request.AuthID = id
				invoke := handleExecExecute
				rawReq := mustJSON(executorRequestWire{ExecutorRequest: request})
				if stream {
					invoke = handleExecStream
					rawReq = mustJSON(executorStreamRequest{ExecutorRequest: request})
				}
				for attempt := 0; attempt < 2; attempt++ {
					raw, err := invoke(rawReq)
					if err != nil {
						raw = errorEnvelopeFor(err)
					}
					var env envelope
					_ = json.Unmarshal(raw, &env)
					if env.OK || env.Error == nil || env.Error.Code != "workbuddy_quota_hold" || env.Error.HTTPStatus != 0 {
						t.Fatalf("unsafe executor error on attempt %d: %s", attempt, raw)
					}
				}
				if calls != 1 {
					t.Fatalf("cached selection bypassed execution guard: %d calls", calls)
				}
			})
		}
	}
}

func TestQuotaMessageOnlyFramesRemainNarrow(t *testing.T) {
	for _, body := range []string{
		`{"msg":"每日请求额度已用尽，明天再来"}`,
		`{"message":"daily usage allowance exhausted"}`,
		`{"message":"shared credits exhausted"}`,
	} {
		if failure := chatStreamFrameError(body); failure == nil || failure.ErrorCode() != "workbuddy_quota_hold" {
			t.Fatalf("confirmed message-only rejection missed: %s", body)
		}
	}
	for _, body := range []string{
		`{"msg":"daily usage allowance status unknown"}`,
		`{"code":0,"message":"daily quota reached"}`,
		`{"choices":[{"delta":{"content":"daily quota reached"}}]}`,
	} {
		if failure := chatStreamFrameError(body); failure != nil {
			t.Fatalf("non-error content falsely classified: %s", body)
		}
	}
}
