package main

import (
	"net/http"
	"net/http/httptest"
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
// Two assertions: concurrent access does not trip the race detector, and
// exactly ONE goroutine wins. The second is the subtler one - a separate
// check and record would be race-free and still emit several Events.
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

func TestScrapeBudget(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		// No header: a curl, a health check, anything not Prometheus.
		{"absent", "", cycleTimeout},
		// The shape Prometheus actually sends.
		{"prometheus default", "10.000000", 10*time.Second - scrapeTimeoutOffset},
		// An operator who raised scrape_timeout for this job gets what they set.
		{"raised", "60", 60*time.Second - scrapeTimeoutOffset},
		// Garbage is not a reason to run unbounded.
		{"unparseable", "soon", cycleTimeout},
		// Shorter than the offset: use it whole rather than going negative.
		{"tighter than the offset", "0.2", 200 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.header != "" {
				r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", tc.header)
			}
			if got := scrapeBudget(r); got != tc.want {
				t.Errorf("header %q: want %v, got %v", tc.header, tc.want, got)
			}
		})
	}
}
