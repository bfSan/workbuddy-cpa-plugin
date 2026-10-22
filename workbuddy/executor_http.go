package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type executorHTTPRequestWire struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func handleExecHTTPRequest(raw []byte) ([]byte, error) {
	var req executorHTTPRequestWire
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if blocked := guardExecutorReadiness(req.AuthID); blocked != nil {
		return blocked, nil
	}
	method := strings.TrimSpace(req.Method)
	if method == "" {
		return nil, fmt.Errorf("executor HTTP request: method is required")
	}
	sa, err := parseStored(req.StorageJSON)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	target, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: invalid URL: %w", err)
	}
	if err := validateExecutorHTTPURL(target, sa); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(method, target.String(), bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	httpReq.Header = req.Headers.Clone()
	if httpReq.Header == nil {
		httpReq.Header = make(http.Header)
	}
	backendHeaders(httpReq, sa)
	// The generic transport can also carry chat requests. Do not let it bypass
	// plugin-owned holds, but never apply them to billing/catalog/OAuth calls.
	isChat := executorHTTPIsChat(target, sa)
	model := ""
	if isChat {
		model = executorHTTPModel(req.Body)
		if blocked := guardExecutorQuota(req.AuthID, model); blocked != nil {
			return blocked, nil
		}
	}
	resp, err := hostHTTPDoWithCallback(httpReq, req.HostCallbackID)
	if err != nil {
		return nil, fmt.Errorf("executor HTTP request: %w", err)
	}
	if isChat {
		if failure := executorHTTPChatFailure(req.AuthID, model, resp.StatusCode, resp.Body, resp.Headers); failure != nil {
			return errorEnvelopeFor(failure), nil
		}
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

func validateExecutorHTTPURL(target *url.URL, sa *storedAuth) error {
	if target == nil || !target.IsAbs() || target.User != nil || target.Fragment != "" {
		return fmt.Errorf("executor HTTP request: absolute URL without userinfo or fragment is required")
	}
	expected, err := url.Parse(upstreamBaseFor(sa))
	if err != nil {
		return fmt.Errorf("executor HTTP request: invalid upstream base: %w", err)
	}
	if !strings.EqualFold(target.Scheme, expected.Scheme) ||
		!strings.EqualFold(target.Hostname(), expected.Hostname()) ||
		target.Port() != expected.Port() {
		return fmt.Errorf("executor HTTP request: upstream host %q is not allowed", target.Host)
	}
	return nil
}

// validateExecutorHTTPURL must run before this path comparison; a foreign realm
// must never become eligible merely by spelling the same chat path.
func executorHTTPIsChat(target *url.URL, sa *storedAuth) bool {
	expected, err := url.Parse(endpointChatFor(sa))
	return err == nil && target != nil && target.Path == expected.Path
}

func executorHTTPModel(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &obj) != nil {
		return ""
	}
	// This is already an upstream HTTP payload. Do not guess OAuth aliases or
	// route across realms; a missing/invalid model permits only account guards.
	return strings.TrimSpace(obj.Model)
}

func executorHTTPChatFailure(authID, model string, status int, body []byte, headers http.Header) *upstreamStatusError {
	if status >= 400 {
		recordUpstreamFailureWithHeaders(authID, model, status, string(body), headers)
		return chatUpstreamError(status, string(body))
	}
	if failure := chatStreamFrameError(string(body)); failure != nil {
		recordUpstreamFailureWithHeaders(authID, model, status, string(body), headers)
		return failure
	}
	for _, line := range strings.Split(string(body), "\n") {
		content := stripDataPrefix(line)
		if failure := chatStreamFrameError(content); failure != nil {
			recordUpstreamFailureWithHeaders(authID, model, status, content, headers)
			return failure
		}
	}
	return nil
}
