package main

import (
	"sync"
	"testing"
	"time"
)

func TestScheduledActionsAtKeepaliveOnlyHour(t *testing.T) {
	runCheckin, runKeepalive, runCatalog := scheduledActionsFor(time.Date(2026, 8, 18, 22, 0, 0, 0, time.Local))
	if runCheckin {
		t.Fatal("22:00 keepalive tick must not run auto check-in")
	}
	if !runKeepalive {
		t.Fatal("22:00 should run keepalive")
	}
	if runCatalog {
		t.Fatal("22:00 should not run the catalog refresh")
	}
}

func TestScheduledActionsAtCheckinHour(t *testing.T) {
	runCheckin, runKeepalive, runCatalog := scheduledActionsFor(time.Date(2026, 8, 18, 21, 0, 0, 0, time.Local))
	if !runCheckin {
		t.Fatal("21:00 should run auto check-in")
	}
	if runKeepalive {
		t.Fatal("21:00 should not run keepalive")
	}
	if runCatalog {
		t.Fatal("21:00 should not run the catalog refresh")
	}
}

// TestScheduledActionsAtCatalogRefreshHour pins the nightly multiplier refresh:
// the upstream updates per-model credit multipliers late in the day, so 23:00
// re-discovers the catalog. It must not collide with the 21:00 check-in or the
// 22:00 keepalive tick.
func TestScheduledActionsAtCatalogRefreshHour(t *testing.T) {
	runCheckin, runKeepalive, runCatalog := scheduledActionsFor(time.Date(2026, 8, 18, 23, 0, 0, 0, time.Local))
	if !runCatalog {
		t.Fatal("23:00 should re-discover the model catalog so multipliers are current")
	}
	if runCheckin {
		t.Fatal("23:00 must not run auto check-in")
	}
	if runKeepalive {
		t.Fatal("23:00 must not run keepalive")
	}
	// The whole hour runs it, matching checkinHours/keepaliveHours semantics.
	if _, _, again := scheduledActionsFor(time.Date(2026, 8, 18, 23, 45, 0, 0, time.Local)); !again {
		t.Fatal("23:45 is still inside the 23:00 refresh slot")
	}
	// And it must not fire a minute early.
	if _, _, early := scheduledActionsFor(time.Date(2026, 8, 18, 22, 59, 0, 0, time.Local)); early {
		t.Fatal("22:59 is not yet the refresh slot")
	}
}

func TestWithCheckinLockSerializesByAuthIndex(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		withCheckinLock("auth-1", func() {
			close(entered)
			<-release
		})
	}()
	<-entered
	locked := make(chan struct{})
	go func() {
		withCheckinLock("auth-1", func() { close(locked) })
	}()
	select {
	case <-locked:
		t.Fatal("second lock holder entered before first released")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	wg.Wait()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("second lock holder did not enter after release")
	}
}
