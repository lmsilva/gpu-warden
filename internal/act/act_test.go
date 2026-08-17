package act

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestClaimRespectsWindow is the behaviour --watch already relied on:
// a job Evented once is not Evented again until the window has passed.
func TestClaimRespectsWindow(t *testing.T) {
	e := NewLog()
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

// TestClaimIsSafeUnderConcurrency is the regression test for the
// crash. Run it with -race.
//
// Two assertions: concurrent access does not trip the race detector, and
// exactly ONE goroutine wins. The second is the subtler one - a separate
// check and record would be race-free and still emit several Events.
func TestClaimIsSafeUnderConcurrency(t *testing.T) {
	const goroutines = 64
	e := NewLog()

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

// TestDistinctJobsUnderConcurrency checks the map itself is not
// corrupted when many different keys are written at once — the write pattern
// a large cluster produces.
func TestDistinctJobsUnderConcurrency(t *testing.T) {
	const jobs = 200
	e := NewLog()

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
