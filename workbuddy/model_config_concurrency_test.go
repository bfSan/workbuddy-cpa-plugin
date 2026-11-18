package main

import (
	"testing"
	"time"
)

func TestSyncHiddenModelsConfigCannotOverwriteConcurrentReconfigure(t *testing.T) {
	oldRuntime := activeModelRuntime.Load()
	oldFeatures := currentFeatureRuntime()
	runtime := newModelRuntime(newModelStore(t.TempDir()), nil)
	activeModelRuntime.Store(runtime)
	t.Cleanup(func() {
		activeModelRuntime.Store(oldRuntime)
		featureRuntime.Store(oldFeatures)
	})
	before, err := parseFeatureRuntime([]byte("wire_version_cli: 1.1.1\nhidden_models: [old]\n"))
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseFeatureRuntime([]byte("wire_version_cli: 2.2.2\nhidden_models: [new]\n"))
	if err != nil {
		t.Fatal(err)
	}
	featureRuntime.Store(before)

	runtime.configCommitMu.Lock()
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		syncHiddenModelsConfig([]string{"panel-hide"})
		close(done)
	}()
	<-started
	completedWhileCommitLocked := false
	select {
	case <-done:
		completedWhileCommitLocked = true
	case <-time.After(20 * time.Millisecond):
	}
	// This store models the publication performed by commitFeatureRuntime while
	// it owns configCommitMu. The waiting overlay edit must clone this latest
	// snapshot only after the commit lock is released.
	featureRuntime.Store(after)
	runtime.configCommitMu.Unlock()
	<-done
	if completedWhileCommitLocked {
		t.Fatal("hidden-model sync bypassed configCommitMu")
	}
	got := currentFeatureRuntime()
	if got.wireVersionCLI != "2.2.2" || len(got.hiddenModels) != 1 || got.hiddenModels[0] != "panel-hide" {
		t.Fatalf("merged feature snapshot = %#v", got)
	}
}
