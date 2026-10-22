package main

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

// These payloads previously passed validatedCheckinStatus when active=false,
// then jsonBool converted the malformed checked flag into a fabricated success.
func TestCheckinReviewMalformedFlagsNeverConfirm(t *testing.T) {
	for _, data := range []string{
		`{"active":false,"today_checked_in":2}`,
		`{"active":false,"today_checked_in":"1"}`,
		`{"active":false,"today_checked_in":null}`,
		`{"active":false,"todayCheckedIn":2}`,
		`{"active":"garbage","today_checked_in":true}`,
		`{"active":false,"today_checked_in":false,"todayCheckedIn":2}`,
	} {
		t.Run(data, func(t *testing.T) {
			checkinTestHTTP(t, 200, `{"code":0,"data":`+data+`}`)
			ci, err := fetchCheckinStatusWithCallback(&storedAuth{}, "")
			var ce *checkinRequestError
			if ci != nil || !errors.As(err, &ce) || ce.Kind != "protocol" {
				t.Fatalf("invalid status became authoritative: status=%+v err=%v", ci, err)
			}
			got := confirmedCheckinAttempt(checkinProtocol{
				status: func() (*checkinSummary, error) { return fetchCheckinStatusWithCallback(&storedAuth{}, "") },
				claim:  func() (map[string]any, error) { t.Fatal("claim must not follow malformed status"); return nil, nil },
			}, false)
			if got.Confirmed || got.State != "failed" || got.Retryable {
				t.Fatalf("invalid response reported successful/retryable: %+v", got)
			}
		})
	}
}

func TestCheckinReviewValidMissingFlagsStillWork(t *testing.T) {
	for _, tc := range []struct {
		data    string
		checked bool
	}{
		{`{"active":false}`, false},
		{`{"today_checked_in":true}`, true},
		{`{"active":1,"today_checked_in":0}`, false},
		{`{"active":"true","today_checked_in":"false"}`, false},
	} {
		t.Run(tc.data, func(t *testing.T) {
			checkinTestHTTP(t, 200, `{"code":0,"data":`+tc.data+`}`)
			ci, err := fetchCheckinStatusWithCallback(&storedAuth{}, "")
			if err != nil || ci == nil || ci.TodayCheckedIn != tc.checked {
				t.Fatalf("valid shape rejected: %+v %v", ci, err)
			}
		})
	}
}

func TestCheckinReviewPermanentTransportDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"unknown CA", x509.UnknownAuthorityError{}},
		{"hostname mismatch", x509.HostnameError{}},
		{"invalid certificate", x509.CertificateInvalidError{}},
		{"system roots", x509.SystemRootsError{}},
		{"unsupported URL scheme", errors.New(`unsupported protocol scheme "broken"`)},
		{"malformed host", url.InvalidHostError("invalid-host")},
		{"bad escape", url.EscapeError("%zz")},
		{"unknown error", errors.New("unclassified transport failure")},
		{"cancelled", context.Canceled},
		{"NXDOMAIN", &net.DNSError{Err: "no such host", Name: "invalid.example", IsNotFound: true}},
		{"RPC certificate error", errors.New("host.http.do: x509: certificate rejected")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every http.Client failure gets this net.Error wrapper, even when its
			// underlying cause is permanent. URL words must not affect policy.
			wrapped := &url.Error{Op: "Post", URL: "https://timeout.example/private-token", Err: tc.err}
			if checkinTransportRetryable(wrapped) {
				t.Fatal("permanent failure classified transient")
			}
			checkinTestHTTP(t, 200, `{}`)
			sharedClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, wrapped })}
			_, err := checkinBillingCall(&storedAuth{}, "/v2/billing/meter/checkin-activity-status", "")
			var ce *checkinRequestError
			if !errors.As(err, &ce) || ce.Retryable {
				t.Fatalf("permanent failure scheduled a retry: %+v", err)
			}
			if strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "timeout.example") {
				t.Fatal("sensitive transport error leaked")
			}
		})
	}
}

func TestCheckinReviewTransientTransportStillRetries(t *testing.T) {
	for _, err := range []error{
		context.DeadlineExceeded, io.EOF, io.ErrUnexpectedEOF,
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT,
		&net.DNSError{Err: "temporary DNS failure", IsTemporary: true},
		errors.New("host.http.do: context deadline exceeded"),
		errors.New("host.http.do: read tcp: i/o timeout"),
	} {
		if !checkinTransportRetryable(&url.Error{Op: "Post", URL: "https://example.invalid", Err: err}) {
			t.Fatalf("transient cause lost: %T", err)
		}
	}
}
