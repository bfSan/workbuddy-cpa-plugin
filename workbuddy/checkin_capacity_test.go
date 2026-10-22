package main

import (
	"fmt"
	"testing"
	"time"
)

func TestCheckinCapacityPreservesUncertainClaimsBeforeNetwork(t *testing.T) {
	s := &checkinRunStore{latest: make(map[string]checkinRunRecord)}
	now := time.Now()
	for i := 0; i < checkinLatestLimit; i++ {
		idx := fmt.Sprint(i)
		if !s.reserve(idx, now) {
			t.Fatal("unexpected capacity refusal")
		}
		s.record(idx, idx, "manual", checkinAttemptResult{State: "pending_confirmation", ConfirmOnly: true, Retryable: true}, now)
		s.release(idx)
	}
	if s.reserve("overflow", now) {
		t.Fatal("admitted new work by discarding uncertain POST state")
	}
	if len(s.latest) != checkinLatestLimit || !s.needsConfirmation("0", now) {
		t.Fatal("lost checkpoint")
	}
	// Existing tasks can still finish while no additional accounts are admitted.
	if !s.reserve("0", now) {
		t.Fatal("capacity prevents completing existing work")
	}
	if s.reserve("0", now) {
		t.Fatal("duplicate in-flight reservation")
	}
	s.record("0", "0", "manual", checkinAttemptResult{State: "already", Confirmed: true}, now)
	if s.reserve("overflow", now) {
		t.Fatal("evicted an in-flight result")
	}
	s.release("0")
	if !s.reserve("overflow", now) {
		t.Fatal("terminal safe record was not reusable")
	}
	s.release("overflow")
}

func TestCheckinCapacityDoesNotResetRetryBudget(t *testing.T) {
	s := &checkinRunStore{latest: make(map[string]checkinRunRecord)}
	now := time.Now()
	for i := 0; i < checkinLatestLimit; i++ {
		idx := fmt.Sprint(i)
		s.latest[idx] = checkinRunRecord{AuthIndex: idx, Day: now.Format("2006-01-02"), At: now, RetryCount: 3, State: "failed"}
	}
	if s.reserve("overflow", now) {
		t.Fatal("evicted same-day budget, allowing unbounded retries")
	}
	if !s.reserve("overflow", now.Add(24*time.Hour)) {
		t.Fatal("previous-day budgets were not reusable")
	}
}

func TestCheckinFailureKeepsUncertainCheckpoint(t *testing.T) {
	s := &checkinRunStore{latest: make(map[string]checkinRunRecord)}
	now := time.Now()
	s.record("a", "a", "manual", checkinAttemptResult{State: "pending_confirmation", ConfirmOnly: true}, now)
	s.record("a", "a", "manual", checkinAttemptResult{State: "failed", Message: "load auth failed"}, now)
	if !s.needsConfirmation("a", now) {
		t.Fatal("auth error discarded uncertain claim")
	}
}

func TestCheckinCacheDoesNotRefreshStaleCredits(t *testing.T) {
	id := "checkin-stale-credits-test"
	at := time.Now().Add(-time.Hour)
	cr := &creditsSummary{TotalRemain: 100, FetchedAt: at.UTC().Format(time.RFC3339)}
	accountCache.Store(id, &accountCacheEntry{credits: cr, plan: "free", fetched: at})
	t.Cleanup(func() { accountCache.Delete(id) })
	mergeCheckinCache(id, &checkinSummary{TodayCheckedIn: true})
	v, _ := accountCache.Load(id)
	got := v.(*accountCacheEntry)
	if !got.fetched.Equal(at) || got.credits.FetchedAt != cr.FetchedAt {
		t.Fatal("checkin made old credit snapshot look fresh")
	}
	if !got.checkin.TodayCheckedIn || got.checkinAt.IsZero() {
		t.Fatal("missing confirmed checkin")
	}
}

func TestCheckinCacheNewConfirmationSurvivesOlderDashboardFlight(t *testing.T) {
	id := "checkin-dashboard-race-test"
	t.Cleanup(func() { accountCache.Delete(id) })
	started := time.Now().Add(-time.Second)
	mergeCheckinCache(id, &checkinSummary{TodayCheckedIn: true})
	got := storeAccountSnapshot(id, &accountCacheEntry{checkin: &checkinSummary{Active: true}, checkinAt: started, credits: &creditsSummary{TotalRemain: 200}, fetched: time.Now()})
	if !got.checkin.TodayCheckedIn || got.credits.TotalRemain != 200 {
		t.Fatal("old flight overwrote confirmation or lost new credits")
	}
	// A later real status is authoritative (for example the next daily window).
	got = storeAccountSnapshot(id, &accountCacheEntry{checkin: &checkinSummary{Active: true}, checkinAt: time.Now().Add(time.Second), fetched: time.Now()})
	if got.checkin.TodayCheckedIn {
		t.Fatal("fresh authoritative status cannot replace old confirmation")
	}
}
