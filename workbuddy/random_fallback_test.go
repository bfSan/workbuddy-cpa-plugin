package main

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type alwaysFailReader struct{}

func (alwaysFailReader) Read([]byte) (int, error) { return 0, errors.New("injected entropy failure") }

func TestRandomFallbackUsesInjectedFailureWithoutGlobalRandMutation(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(0xa0 + i)
	}
	var firstCounter atomic.Uint64
	first := randomBytesWithFallback(alwaysFailReader{}, 80, key, &firstCounter)
	second := randomBytesWithFallback(alwaysFailReader{}, 80, key, &firstCounter)
	if len(first) != 80 || len(second) != 80 || bytes.Equal(first, make([]byte, 80)) {
		t.Fatalf("invalid fallback output: first=%x second=%x", first, second)
	}
	if bytes.Equal(first, second) {
		t.Fatal("fallback counter reused output")
	}
	var replayCounter atomic.Uint64
	replay := randomBytesWithFallback(alwaysFailReader{}, 80, key, &replayCounter)
	if !bytes.Equal(first, replay) {
		t.Fatal("injected fallback was not deterministic for the same key/counter")
	}
}

func TestRandomFallbackConcurrentOutputsAreUnique(t *testing.T) {
	var key [32]byte
	for i := range key {
		key[i] = byte(i + 1)
	}
	var counter atomic.Uint64
	const workers = 128
	outputs := make(chan string, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			outputs <- string(randomBytesWithFallback(alwaysFailReader{}, 32, key, &counter))
		}()
	}
	wg.Wait()
	close(outputs)
	seen := make(map[string]struct{}, workers)
	for output := range outputs {
		if _, duplicate := seen[output]; duplicate {
			t.Fatal("concurrent fallback output was reused")
		}
		seen[output] = struct{}{}
	}
	if len(seen) != workers || counter.Load() != workers {
		t.Fatalf("unique=%d counter=%d", len(seen), counter.Load())
	}
}

func TestRandomBytesUsesReaderBeforeFallback(t *testing.T) {
	input := bytes.Repeat([]byte{0x5a}, 16)
	var counter atomic.Uint64
	got := randomBytesWithFallback(bytes.NewReader(input), len(input), [32]byte{}, &counter)
	if !bytes.Equal(got, input) || counter.Load() != 0 {
		t.Fatalf("got=%x counter=%d", got, counter.Load())
	}
}

func TestStartupFallbackKeyFailsClosedOnEntropyFailure(t *testing.T) {
	if _, err := randomFallbackKeyFrom(alwaysFailReader{}); err == nil {
		t.Fatal("startup key unexpectedly succeeded")
	}
}
