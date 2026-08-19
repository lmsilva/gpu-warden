package cycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
)

// fakeBuilder returns whatever it is given, and counts how often it was asked.
type fakeBuilder struct {
	reports []report.JobReport
	queue   report.Queue
	err     error
	calls   int
}

func (f *fakeBuilder) Build(context.Context) ([]report.JobReport, report.Queue, error) {
	f.calls++
	return f.reports, f.queue, f.err
}

// idleHolder is a job past its grace period holding four GPUs with one lit -
// the shape both allocation findings key on.
func idleHolder(grace time.Duration) report.JobReport {
	return report.JobReport{
		Job:     slurmapi.Job{JobID: 7, Name: "holder"},
		GPUs:    4,
		Elapsed: grace + time.Minute,
		PerGPU:  true,
		HasLit:  true, LitGPUs: 1,
	}
}

// TestBuildCarriesTheQueueIntoTheEngine covers the one line nothing else can:
// the copy from report.Queue into cross.Queue. Two int fields of the same
// type, side by side, in the order a transposition would look correct - and
// the only symptom would be a finding quoting plausible wrong numbers.
func TestBuildCarriesTheQueueIntoTheEngine(t *testing.T) {
	grace := verdict.DefaultThresholds().GraceCeiling
	b := &fakeBuilder{
		reports: []report.JobReport{idleHolder(grace)},
		queue:   report.Queue{PendingGPUJobs: 2, PendingGPUs: 9},
	}
	s, err := Build(context.Background(), b, grace)
	if err != nil {
		t.Fatal(err)
	}

	var blocking string
	for _, f := range s.Findings {
		if f.Rule == "blocking-idle-allocation" {
			blocking = f.Message
		}
	}
	if blocking == "" {
		t.Fatalf("idle devices and a non-empty queue must produce the finding, got %v", s.Findings)
	}
	// 2 jobs waiting for 9 GPUs, not 9 jobs waiting for 2.
	if want := "2 jobs wait for 9 GPUs"; !strings.Contains(blocking, want) {
		t.Errorf("queue numbers transposed or reworded: %q", blocking)
	}
}

// TestBuildWithNothingWaiting is the negative half. The allocation is still
// reported; only the claim that somebody is waiting for it goes away.
func TestBuildWithNothingWaiting(t *testing.T) {
	grace := verdict.DefaultThresholds().GraceCeiling
	b := &fakeBuilder{reports: []report.JobReport{idleHolder(grace)}}
	s, err := Build(context.Background(), b, grace)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]bool{}
	for _, f := range s.Findings {
		rules[f.Rule] = true
	}
	if rules["blocking-idle-allocation"] {
		t.Error("nothing waiting means nothing is blocked")
	}
	if !rules["partially-used-allocation"] {
		t.Errorf("the underlying finding must still stand on its own, got %v", s.Findings)
	}
}

// TestBuildStampsTheBuildTime pins what At means. It is when the pass
// finished, and a presenter showing "as of" reads it - so a snapshot with a
// zero time would render an epoch date rather than a time.
func TestBuildStampsTheBuildTime(t *testing.T) {
	before := time.Now()
	s, err := Build(context.Background(), &fakeBuilder{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if s.At.Before(before) || s.At.After(time.Now()) {
		t.Errorf("At %v is not within the call, which ran between %v and now", s.At, before)
	}
}

// TestBuildPassesTheErrorThrough checks a failed join produces no snapshot
// rather than an empty one that reads like an idle cluster.
func TestBuildPassesTheErrorThrough(t *testing.T) {
	b := &fakeBuilder{err: errors.New("slurmrestd unreachable")}
	if _, err := Build(context.Background(), b, time.Minute); err == nil {
		t.Fatal("want the builder's error")
	}
}

// fixedSnapshot is a stand-in pass. Its contents do not matter; only how many
// times the source ran does.
func fixedSnapshot() Snapshot {
	return Snapshot{Reports: []report.JobReport{{GPUs: 1, HasData: true}}}
}

// TestCacheServesWithinWindow proves the amplification fix: repeated callers
// inside minAge cost exactly one fan-out.
func TestCacheServesWithinWindow(t *testing.T) {
	var builds int
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds++
		return fixedSnapshot(), nil
	}), time.Hour)

	for i := 0; i < 10; i++ {
		got, err := c.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if len(got.Reports) != 1 {
			t.Fatalf("call %d: want 1 report, got %d", i, len(got.Reports))
		}
	}
	if builds != 1 {
		t.Errorf("10 calls inside the window must cost 1 build, got %d", builds)
	}
}

// TestCacheRebuildsAfterWindow proves the cache expires rather than pinning
// stale data forever.
func TestCacheRebuildsAfterWindow(t *testing.T) {
	var builds int
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds++
		return fixedSnapshot(), nil
	}), time.Millisecond)

	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if builds != 2 {
		t.Errorf("a call after the window must rebuild, got %d builds", builds)
	}
}

// TestCacheDisabled checks that --serve-cache 0 restores the old
// build-every-request behaviour, for anyone who wants a reading taken at
// request time and accepts the cost.
func TestCacheDisabled(t *testing.T) {
	var builds int
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds++
		return fixedSnapshot(), nil
	}), 0)

	for i := 0; i < 3; i++ {
		if _, err := c.Snapshot(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if builds != 3 {
		t.Errorf("a zero window must rebuild every time, got %d builds", builds)
	}
}

// TestCacheDoesNotCacheErrors is the one that matters operationally: a failed
// build must not suppress the next attempt, or one transient slurmrestd
// hiccup would blind every surface for a whole window.
func TestCacheDoesNotCacheErrors(t *testing.T) {
	// Two failures, then a success: the recovery is part of the same
	// sequence, so nothing has to reach into the cache to arrange it.
	var builds int
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds++
		if builds <= 2 {
			return Snapshot{}, errors.New("slurmrestd unreachable")
		}
		return fixedSnapshot(), nil
	}), time.Hour)

	if _, err := c.Snapshot(context.Background()); err == nil {
		t.Fatal("want an error from a failing build")
	}
	if _, err := c.Snapshot(context.Background()); err == nil {
		t.Fatal("want an error from the second failing build too")
	}
	if builds != 2 {
		t.Errorf("errors must not be cached: want 2 builds, got %d", builds)
	}
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Errorf("recovery build failed: %v", err)
	}
}

// TestCacheCollapsesConcurrentCallers is the second half of the amplification
// fix. Twenty simultaneous callers must produce ONE fan-out: a bare time
// check would let all twenty read the same stale timestamp.
//
// Run with -race.
func TestCacheCollapsesConcurrentCallers(t *testing.T) {
	const callers = 20
	var builds atomic.Int64
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds.Add(1)
		// Long enough that the others are certainly waiting on the lock.
		time.Sleep(20 * time.Millisecond)
		return fixedSnapshot(), nil
	}), time.Hour)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := c.Snapshot(context.Background()); err != nil {
				t.Errorf("concurrent call failed: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Errorf("%d concurrent calls must collapse into 1 build, got %d", callers, got)
	}
}

// TestCacheServesEmptyBuilds is the idle-cluster case. A pass over a cluster
// with no GPU jobs produces no reports, so a cache testing the contents
// rather than the timestamp would never cache anything here and every request
// would run the full fan-out.
func TestCacheServesEmptyBuilds(t *testing.T) {
	var builds atomic.Int64
	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		builds.Add(1)
		return Snapshot{}, nil // an idle cluster: no GPU jobs, no error
	}), time.Hour)

	for i := 0; i < 5; i++ {
		got, err := c.Snapshot(context.Background())
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
		if len(got.Reports) != 0 {
			t.Errorf("call %d: want no reports, got %d", i, len(got.Reports))
		}
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("an empty build must still be cached: want 1 build, got %d", got)
	}
}

// TestCacheWaiterHonoursContext pins the reason sem is a channel and not a
// sync.Mutex. A caller waiting behind a slow build must give up when its own
// context does, instead of holding a goroutine for a client that has gone.
func TestCacheWaiterHonoursContext(t *testing.T) {
	// Hold the slot open so the waiter below is certainly blocked on it.
	release := make(chan struct{})
	holding := make(chan struct{})
	var built atomic.Int64

	c := NewCache(SourceFunc(func(context.Context) (Snapshot, error) {
		built.Add(1)
		close(holding)
		<-release
		return fixedSnapshot(), nil
	}), time.Hour)

	go func() { _, _ = c.Snapshot(context.Background()) }()
	<-holding
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Snapshot(ctx)
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
	if got := built.Load(); got != 1 {
		t.Errorf("a cancelled waiter must not build: want only the held build, got %d", got)
	}
}
