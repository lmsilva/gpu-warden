package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/report"
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

// fixedReports is a stand-in build result. Its contents do not matter; only
// how many times the builder ran does.
func fixedReports() []report.JobReport {
	return []report.JobReport{{GPUs: 1, HasData: true}}
}

// TestCycleCacheServesWithinWindow proves the amplification fix: repeated
// scrapes inside minAge cost exactly one fan-out.
func TestCycleCacheServesWithinWindow(t *testing.T) {
	c := newCycleCache(time.Hour)
	var builds int
	build := func(context.Context) (cycle, error) {
		builds++
		return cycle{reports: fixedReports()}, nil
	}
	for i := 0; i < 10; i++ {
		got, err := c.get(context.Background(), build)
		if err != nil {
			t.Fatalf("scrape %d: unexpected error: %v", i, err)
		}
		if len(got.reports) != 1 {
			t.Fatalf("scrape %d: want 1 report, got %d", i, len(got.reports))
		}
	}
	if builds != 1 {
		t.Errorf("10 scrapes inside the window must cost 1 build, got %d", builds)
	}
}

// TestCycleCacheRebuildsAfterWindow proves the cache expires rather than
// pinning stale data forever.
func TestCycleCacheRebuildsAfterWindow(t *testing.T) {
	c := newCycleCache(time.Millisecond)
	var builds int
	build := func(context.Context) (cycle, error) {
		builds++
		return cycle{reports: fixedReports()}, nil
	}
	if _, err := c.get(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.get(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Errorf("a scrape after the window must rebuild, got %d builds", builds)
	}
}

// TestCycleCacheDisabled checks that --serve-cache 0 restores the old
// build-every-scrape behaviour, for anyone who wants a reading taken at scrape
// time and accepts the cost.
func TestCycleCacheDisabled(t *testing.T) {
	c := newCycleCache(0)
	var builds int
	build := func(context.Context) (cycle, error) {
		builds++
		return cycle{reports: fixedReports()}, nil
	}
	for i := 0; i < 3; i++ {
		if _, err := c.get(context.Background(), build); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 3 {
		t.Errorf("a zero window must rebuild every time, got %d builds", builds)
	}
}

// TestCycleCacheDoesNotCacheErrors is the one that matters operationally: a
// failed build must not suppress the next attempt, or one transient slurmrestd
// hiccup would blind the exporter for a whole window.
func TestCycleCacheDoesNotCacheErrors(t *testing.T) {
	c := newCycleCache(time.Hour)
	var builds int
	failing := func(context.Context) (cycle, error) {
		builds++
		return cycle{}, errors.New("slurmrestd unreachable")
	}
	if _, err := c.get(context.Background(), failing); err == nil {
		t.Fatal("want an error from a failing build")
	}
	if _, err := c.get(context.Background(), failing); err == nil {
		t.Fatal("want an error from the second failing build too")
	}
	if builds != 2 {
		t.Errorf("errors must not be cached: want 2 builds, got %d", builds)
	}

	// And a success right after a failure must be served normally.
	if _, err := c.get(context.Background(), func(context.Context) (cycle, error) {
		return cycle{reports: fixedReports()}, nil
	}); err != nil {
		t.Errorf("recovery build failed: %v", err)
	}
}

// TestCycleCacheCollapsesConcurrentScrapes is the second half of the
// amplification fix. Twenty simultaneous scrapes must produce ONE fan-out:
// a bare time check would let all twenty read the same stale timestamp.
//
// Run with -race.
func TestCycleCacheCollapsesConcurrentScrapes(t *testing.T) {
	const scrapes = 20
	c := newCycleCache(time.Hour)

	var builds atomic.Int64
	build := func(context.Context) (cycle, error) {
		builds.Add(1)
		// Long enough that the others are certainly waiting on the lock.
		time.Sleep(20 * time.Millisecond)
		return cycle{reports: fixedReports()}, nil
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < scrapes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := c.get(context.Background(), build); err != nil {
				t.Errorf("concurrent scrape failed: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Errorf("%d concurrent scrapes must collapse into 1 build, got %d",
			scrapes, got)
	}
}

// TestCycleCacheServesEmptyBuilds is the idle-cluster case. report.Build
// returns a NIL slice when no GPU jobs are running, so a cache testing
// c.reports != nil would never cache anything here and every scrape would
// run the full fan-out.
func TestCycleCacheServesEmptyBuilds(t *testing.T) {
	c := newCycleCache(time.Hour)

	var builds atomic.Int64
	build := func(context.Context) (cycle, error) {
		builds.Add(1)
		return cycle{}, nil // an idle cluster: no GPU jobs, no error
	}

	for i := 0; i < 5; i++ {
		got, err := c.get(context.Background(), build)
		if err != nil {
			t.Fatalf("scrape %d failed: %v", i, err)
		}
		if len(got.reports) != 0 {
			t.Errorf("scrape %d: want no reports, got %d", i, len(got.reports))
		}
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("an empty build must still be cached: want 1 build, got %d", got)
	}
}

// TestCycleCacheWaiterHonoursContext pins the reason sem is a channel and not
// a sync.Mutex. A scrape waiting behind a slow build must give up when its own
// context does, instead of holding a goroutine for a client that has gone.
func TestCycleCacheWaiterHonoursContext(t *testing.T) {
	c := newCycleCache(time.Hour)

	// Hold the slot open so the waiter below is certainly blocked on it.
	release := make(chan struct{})
	holding := make(chan struct{})
	go func() {
		_, _ = c.get(context.Background(), func(context.Context) (cycle, error) {
			close(holding)
			<-release
			return cycle{reports: fixedReports()}, nil
		})
	}()
	<-holding
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var built atomic.Int64
	done := make(chan error, 1)
	go func() {
		_, err := c.get(ctx, func(context.Context) (cycle, error) {
			built.Add(1)
			return cycle{reports: fixedReports()}, nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not observe its cancelled context: sem is behaving like a mutex")
	}
	if got := built.Load(); got != 0 {
		t.Errorf("a cancelled waiter must not build, got %d builds", got)
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
