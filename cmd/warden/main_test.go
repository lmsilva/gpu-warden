package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEventLogClaimRespectsWindow is the behaviour --watch already relied on:
// a job Evented once is not Evented again until the window has passed.
func TestEventLogClaimRespectsWindow(t *testing.T) {
	e := newEventLog()
	if !e.claim(42, time.Hour) {
		t.Fatal("first claim on an unseen job must succeed")
	}
	if e.claim(42, time.Hour) {
		t.Error("second claim inside the window must be refused")
	}
	if !e.claim(43, time.Hour) {
		t.Error("a different job must not be blocked by another job's claim")
	}
	// A zero window means every claim is outside it.
	if !e.claim(42, 0) {
		t.Error("claim with a zero window must always succeed")
	}
}

// TestEventLogClaimIsSafeUnderConcurrency is the regression test for the
// crash. Run it with -race.
//
// Two things are asserted. The obvious one is that concurrent access does not
// trip Go's race detector or the runtime's concurrent-map-write panic, which
// is what an unguarded map did the moment --serve and --act were combined. The
// subtler one is that exactly ONE goroutine wins: if the check and the record
// were not under a single lock, several would pass the check together and
// several Events would be created for one job.
func TestEventLogClaimIsSafeUnderConcurrency(t *testing.T) {
	const goroutines = 64
	e := newEventLog()

	var claimed atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to actually contend
			if e.claim(1, time.Hour) {
				claimed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := claimed.Load(); got != 1 {
		t.Errorf("want exactly 1 successful claim across %d goroutines, got %d",
			goroutines, got)
	}
}

// TestEventLogDistinctJobsUnderConcurrency checks the map itself is not
// corrupted when many different keys are written at once — the write pattern
// a large cluster produces.
func TestEventLogDistinctJobsUnderConcurrency(t *testing.T) {
	const jobs = 200
	e := newEventLog()

	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			e.claim(id, time.Hour)
		}(i)
	}
	wg.Wait()

	for i := 0; i < jobs; i++ {
		if e.claim(i, time.Hour) {
			t.Fatalf("job %d was not recorded by its concurrent claim", i)
		}
	}
}
