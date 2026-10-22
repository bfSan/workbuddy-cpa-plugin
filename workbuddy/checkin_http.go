package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Dedicated check-in transport: one attempt per operation. Generic billing
// retries must not replay a mutating claim or turn a missing code into code=0.
type checkinRequestError struct {
	Kind      string
	Status    int
	Retryable bool
	Uncertain bool // a POST transport failure may already have created a grant
}

func (e *checkinRequestError) Error() string {
	switch e.Kind {
	case "auth":
		return "登录已失效或无权签到，请重新登录；不会自动重试"
	case "transport":
		return "签到连接失败或超时，将有限次数重试/核实"
	case "rate_limit":
		return "签到接口暂时限流，将有限次数重试"
	case "server":
		return "签到服务暂时不可用，将有限次数重试"
	case "already":
		return "上游提示已签到，仍需查询状态确认"
	case "protocol":
		return "签到响应格式异常，未确认成功"
	case "not_found":
		return "签到接口不存在"
	default:
		return "上游拒绝签到，请检查账号或活动资格；不会自动重试"
	}
}

func checkinBillingCall(sa *storedAuth, path, callbackID string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, billingBaseFor(sa)+path, strings.NewReader("{}"))
	if err != nil {
		return nil, &checkinRequestError{Kind: "protocol"}
	}
	billingHeaders(req, sa)
	resp, err := hostHTTPDoWithCallback(req, callbackID)
	if err != nil {
		// Blocked proxy and configuration errors must not become retry loops.
		retryable := checkinTransportRetryable(err)
		return nil, &checkinRequestError{Kind: "transport", Retryable: retryable, Uncertain: true}
	}
	if resp == nil {
		return nil, &checkinRequestError{Kind: "protocol", Uncertain: true}
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, &checkinRequestError{Kind: "auth", Status: resp.StatusCode}
	case resp.StatusCode == 404:
		return nil, &checkinRequestError{Kind: "not_found", Status: resp.StatusCode}
	case resp.StatusCode == 429:
		return nil, &checkinRequestError{Kind: "rate_limit", Status: resp.StatusCode, Retryable: true}
	case resp.StatusCode >= 500:
		return nil, &checkinRequestError{Kind: "server", Status: resp.StatusCode, Retryable: true, Uncertain: true}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, &checkinRequestError{Kind: "business", Status: resp.StatusCode}
	}
	var env struct {
		Code *int            `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(resp.Body, &env) != nil || env.Code == nil {
		return nil, &checkinRequestError{Kind: "protocol", Uncertain: true}
	}
	if *env.Code != 0 {
		kind := "business"
		low := strings.ToLower(env.Msg)
		if strings.Contains(low, "already checked in") || strings.Contains(env.Msg, "今日已签到") || strings.Contains(env.Msg, "今天已签到") {
			kind = "already"
		}
		return nil, &checkinRequestError{Kind: kind}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil, &checkinRequestError{Kind: "protocol", Uncertain: true}
	}
	return env.Data, nil
}

func checkinBool(m map[string]any, keys ...string) (bool, bool) {
	for _, key := range keys {
		if v, ok := m[key]; ok {
			switch b := v.(type) {
			case bool:
				return b, true
			case string:
				if b == "true" || b == "false" {
					return b == "true", true
				}
			case float64:
				if b == 0 || b == 1 {
					return b == 1, true
				}
			}
			return false, false
		}
	}
	return false, false
}

func validatedCheckinStatus(m map[string]any) error {
	if m == nil {
		return &checkinRequestError{Kind: "protocol"}
	}
	checked, hasChecked := checkinBool(m, "today_checked_in", "todayCheckedIn")
	active, hasActive := checkinBool(m, "active", "Active")
	// Absent flags may be legitimate (inactive activity or an already-checked
	// snapshot), but present malformed flags must never reach the legacy loose
	// jsonBool conversion, which treats e.g. numeric 2 and string "1" as true.
	for _, key := range []string{"today_checked_in", "todayCheckedIn", "active", "Active"} {
		if _, present := m[key]; present {
			if _, valid := checkinBool(m, key); !valid {
				return &checkinRequestError{Kind: "protocol"}
			}
		}
	}
	if !hasChecked && !(hasActive && !active) || !checked && !hasActive {
		return &checkinRequestError{Kind: "protocol"}
	}
	return nil
}

func checkinStatusHTTPError(err error) int {
	var ce *checkinRequestError
	if errors.As(err, &ce) {
		return ce.Status
	}
	return 0
}

// Avoid returning body/URL/token-containing upstream errors through diagnostics.
func checkinListError() map[string]any {
	return map[string]any{"error": "无法读取签到账号列表"}
}

// url.Error implements net.Error even for permanent URL/certificate failures.
// Inspect its cause, not merely interface membership or the URL text (a URL may
// itself contain "timeout"). RPC text fallback is deliberately narrow.
func checkinTransportRetryable(err error) bool {
	if err == nil {
		return false
	}
	for {
		var ue *url.Error
		if !errors.As(err, &ue) || ue.Err == nil {
			break
		}
		err = ue.Err
	}
	var unknownCA x509.UnknownAuthorityError
	var invalidCert x509.CertificateInvalidError
	var hostname x509.HostnameError
	var roots x509.SystemRootsError
	if errors.As(err, &unknownCA) || errors.As(err, &invalidCert) || errors.As(err, &hostname) || errors.As(err, &roots) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, permanent := range []string{"x509:", "certificate", "unsupported protocol scheme", "invalid url", "invalid port", "invalid host", "no such host"} {
		if strings.Contains(msg, permanent) {
			return false
		}
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, transient := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ETIMEDOUT, syscall.EPIPE, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) {
		return true
	}
	return msg == "timeout" || msg == "eof" || strings.Contains(msg, "i/o timeout") || strings.Contains(msg, "timed out") || strings.Contains(msg, "deadline exceeded") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "unexpected eof")
}
