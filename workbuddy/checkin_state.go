package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

// A successful POST is an acknowledgement, not proof that the benefit exists.
// Only an authoritative status read may set Confirmed/TodayCheckedIn.
type checkinAttemptResult struct {
	State       string
	Message     string
	Confirmed   bool
	Retryable   bool
	ConfirmOnly bool
	Status      *checkinSummary
	Receipt     map[string]any
}

type checkinProtocol struct {
	status func() (*checkinSummary, error)
	claim  func() (map[string]any, error)
}

func confirmedCheckinAttempt(p checkinProtocol, confirmOnly bool) checkinAttemptResult {
	ci, err := p.status()
	if err != nil {
		return checkinAttemptResult{State: "failed", Message: checkinErrorMessage(err), Retryable: checkinErrorRetryable(err), ConfirmOnly: confirmOnly}
	}
	if ci == nil {
		return checkinAttemptResult{State: "failed", Message: "签到状态缺失，未发起领取", ConfirmOnly: confirmOnly}
	}
	if ci.TodayCheckedIn {
		return checkinAttemptResult{State: "already", Message: "今日已签到（上游状态已确认）", Confirmed: true, Status: ci}
	}
	if confirmOnly {
		return checkinAttemptResult{State: "pending_confirmation", Message: "领取结果尚未确认，将有限次数复查；不重复提交领取", Retryable: true, ConfirmOnly: true, Status: ci}
	}
	if !ci.Active {
		return checkinAttemptResult{State: "no_activity", Message: "当前无可签到活动", Status: ci}
	}
	receipt, claimErr := p.claim()
	// Even a timed-out POST may have reached upstream. Never blindly replay it.
	// A definite rejection may be retried, but only after a fresh status read.
	if claimErr != nil && !checkinErrorRetryable(claimErr) && !isCheckinAlreadyError(claimErr) && !checkinErrorUncertain(claimErr) {
		return checkinAttemptResult{State: "failed", Message: checkinErrorMessage(claimErr), Status: ci}
	}
	ci2, verifyErr := p.status()
	if verifyErr == nil && ci2 != nil && ci2.TodayCheckedIn {
		state, message := "success", "签到已确认（上游状态已更新）"
		if isCheckinAlreadyError(claimErr) {
			state, message = "already", "今日已签到（上游状态已确认）"
		}
		return checkinAttemptResult{State: state, Message: message, Confirmed: true, Status: ci2, Receipt: receipt}
	}
	// Preserve a real, unconfirmed status rather than writing a fabricated true.
	if ci2 != nil {
		ci = ci2
	}
	if verifyErr != nil && !checkinErrorRetryable(verifyErr) {
		return checkinAttemptResult{State: "failed", Message: "领取后验证失败：" + checkinErrorMessage(verifyErr), ConfirmOnly: true, Status: ci}
	}
	if claimErr != nil && checkinErrorRetryable(claimErr) {
		var ce *checkinRequestError
		uncertain := !errors.As(claimErr, &ce) || ce.Uncertain
		return checkinAttemptResult{State: "failed", Message: checkinErrorMessage(claimErr), Retryable: true, ConfirmOnly: uncertain, Status: ci}
	}
	if receipt != nil && receipt["success"] == false && !isCheckinAlreadyError(claimErr) {
		return checkinAttemptResult{State: "failed", Message: "上游明确拒绝领取；状态未确认", Status: ci}
	}
	return checkinAttemptResult{State: "pending_confirmation", Message: "上游已受理，签到尚未确认；稍后只复查状态", Retryable: true, ConfirmOnly: true, Status: ci}
}

// Retry work is process-local, bounded per account/day, and never retains a
// request-scoped host callback ID or credentials. Each retry re-reads the auth.
var checkinRetryDelays = [...]time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute}

const checkinHistoryLimit = 100
const checkinLatestLimit = 512

type checkinRunRecord struct {
	AuthIndex    string    `json:"auth_index"`
	AuthID       string    `json:"auth_id,omitempty"`
	Source       string    `json:"source"`
	Day          string    `json:"day"`
	State        string    `json:"state"`
	Message      string    `json:"message"`
	At           time.Time `json:"at"`
	RetryCount   int       `json:"retry_count"`
	NextRetryAt  time.Time `json:"next_retry_at,omitempty"`
	ConfirmOnly  bool      `json:"confirm_only"`
	Confirmed    bool      `json:"confirmed"`
	RetryAllowed bool      `json:"retry_allowed"`
	Manual       bool      `json:"manual"`
}

type checkinRunStore struct {
	mu       sync.Mutex
	latest   map[string]checkinRunRecord
	history  []checkinRunRecord
	inFlight map[string]bool
}

var checkinRuns = &checkinRunStore{latest: make(map[string]checkinRunRecord)}

// reserve must run before any upstream operation. Do not lose an uncertain
// POST checkpoint just because another account starts a new task.
func (s *checkinRunStore) reserve(index string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index == "" {
		return false
	}
	if s.inFlight == nil {
		s.inFlight = make(map[string]bool)
	}
	if s.inFlight[index] {
		return false
	}
	if _, exists := s.latest[index]; !exists {
		if !s.makeRoomLocked(now) {
			return false
		}
		s.latest[index] = checkinRunRecord{AuthIndex: index, Day: now.Format("2006-01-02"), At: now, State: "running"}
	}
	s.inFlight[index] = true
	return true
}

func (s *checkinRunStore) release(index string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, index)
}

func (s *checkinRunStore) makeRoomLocked(now time.Time) bool {
	if len(s.latest) < checkinLatestLimit {
		return true
	}
	var oldest string
	var at time.Time
	for k, v := range s.latest {
		if s.inFlight[k] || v.Day == now.Format("2006-01-02") && (v.ConfirmOnly && !v.Confirmed || v.RetryAllowed || v.RetryCount > 0) {
			continue
		}
		if oldest == "" || v.At.Before(at) {
			oldest, at = k, v.At
		}
	}
	if oldest == "" {
		return false
	}
	delete(s.latest, oldest)
	return true
}

func (s *checkinRunStore) record(index, id, source string, result checkinAttemptResult, now time.Time) checkinRunRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := now.Format("2006-01-02")
	previous, exists := s.latest[index]
	retries := 0
	manual := source == "manual"
	if exists && previous.Day == day {
		retries = previous.RetryCount
		if source == "retry" {
			retries++
			manual = previous.Manual
		}
	}
	r := checkinRunRecord{AuthIndex: index, AuthID: id, Source: source, Day: day, State: result.State, Message: result.Message, At: now, RetryCount: retries, Confirmed: result.Confirmed, ConfirmOnly: result.ConfirmOnly, Manual: manual}
	// A later auth/status failure must not erase an uncertain POST checkpoint.
	if exists && previous.Day == day && previous.ConfirmOnly && !result.Confirmed {
		r.ConfirmOnly = true
	}
	if result.Retryable && retries < len(checkinRetryDelays) {
		next := now.Add(checkinRetryDelays[retries])
		if next.Format("2006-01-02") == day {
			r.NextRetryAt, r.RetryAllowed = next, true
		}
	}
	if !exists && !s.makeRoomLocked(now) {
		return checkinRunRecord{AuthIndex: index, Source: source, Day: day, At: now, State: "failed", Message: "签到任务容量已满，请等待现有任务完成"}
	}
	s.latest[index] = r
	s.history = append(s.history, r)
	if len(s.history) > checkinHistoryLimit {
		s.history = append([]checkinRunRecord(nil), s.history[len(s.history)-checkinHistoryLimit:]...)
	}
	return r
}

func (s *checkinRunStore) snapshot(index string) (latest, history []checkinRunRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, r := range s.latest {
		if index == "" || index == k {
			latest = append(latest, r)
		}
	}
	for _, r := range s.history {
		if index == "" || r.AuthIndex == index {
			history = append(history, r)
		}
	}
	sort.Slice(latest, func(i, j int) bool { return latest[i].At.After(latest[j].At) })
	return
}

func (s *checkinRunStore) due(now time.Time, autoEnabled bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for index, r := range s.latest {
		if r.Day != now.Format("2006-01-02") || !autoEnabled && !r.Manual {
			r.NextRetryAt, r.RetryAllowed = time.Time{}, false
			s.latest[index] = r
			continue
		}
		if r.RetryAllowed && !r.NextRetryAt.IsZero() && !r.NextRetryAt.After(now) {
			out = append(out, index)
		}
	}
	return out
}

func (s *checkinRunStore) needsConfirmation(index string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.latest[index]
	return ok && r.Day == now.Format("2006-01-02") && r.ConfirmOnly && !r.Confirmed
}

func (s *checkinRunStore) retryMode(index string, now time.Time) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.latest[index]
	return r.ConfirmOnly, ok && r.RetryAllowed && r.Day == now.Format("2006-01-02") && !r.NextRetryAt.After(now)
}

func (s *checkinRunStore) cancel(index string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.latest[index]
	if ok {
		r.RetryAllowed, r.NextRetryAt = false, time.Time{}
		s.latest[index] = r
	}
}

func (s *checkinRunStore) rememberResult(index, id, source string, result checkinAttemptResult) map[string]any {
	r := s.record(index, id, source, result, time.Now())
	// Identity is hashed; no upstream bodies, URLs, tokens or login state logged.
	h := sha256.Sum256([]byte(index))
	log.Printf("workbuddy checkin account=%x source=%s state=%s retry=%d scheduled=%t", h[:6], source, r.State, r.RetryCount, r.RetryAllowed)
	out := map[string]any{"auth_index": index, "success": r.Confirmed, "confirmed": r.Confirmed, "state": r.State, "message": r.Message, "retry_count": r.RetryCount, "retry_scheduled": r.RetryAllowed}
	if r.RetryAllowed {
		out["next_retry_at"] = r.NextRetryAt.UTC().Format(time.RFC3339)
	}
	if r.State != "success" {
		out["reason"] = r.State
	}
	if r.State == "already" || r.State == "global" || r.State == "no_activity" || r.State == "disabled" {
		out["skipped"] = true
	}
	// Copy only receipt fields used by the panel, never arbitrary upstream maps.
	if r.Confirmed {
		for _, k := range []string{"credit", "daily_credit", "streak_days"} {
			if n, ok := result.Receipt[k].(float64); ok {
				out[k] = n
			}
		}
	}
	return out
}

func checkinErrorMessage(err error) string {
	var ce *checkinRequestError
	if errors.As(err, &ce) {
		return ce.Error()
	}
	return "签到请求失败，请检查连接或登录状态"
}

func checkinErrorRetryable(err error) bool {
	var ce *checkinRequestError
	return errors.As(err, &ce) && ce.Retryable
}

func checkinErrorUncertain(err error) bool {
	var ce *checkinRequestError
	return errors.As(err, &ce) && ce.Uncertain
}

func isCheckinAlreadyError(err error) bool {
	var ce *checkinRequestError
	return errors.As(err, &ce) && ce.Kind == "already"
}

func handleCheckinStatus(index string) map[string]any {
	latest, history := checkinRuns.snapshot(index)
	checkinAutoMu.RLock()
	enabled := checkinAuto
	checkinAutoMu.RUnlock()
	return map[string]any{"enabled": enabled, "persistent": false, "schedule": checkinHours, "max_retries": len(checkinRetryDelays), "retry_delays_seconds": []int{60, 120, 300}, "latest": latest, "history": history, "note": fmt.Sprintf("记录仅保留本进程最近 %d 次结果；重启清空，不自动补领", checkinHistoryLimit)}
}
