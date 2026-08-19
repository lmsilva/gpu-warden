// Package cycle is one pass over the cluster, and the seam every presenter
// reads through.
//
// A pass produces a Snapshot: the per-job reports, the queue pressure, and
// the findings drawn from both. Presenters take that value and render it -
// none of them calls the builder, derives a finding, or decides how old the
// numbers are. Two consequences follow. The table and the web page cannot
// disagree, because there is nothing left for them to disagree about. And
// what is behind the seam can change - a live build now, something stored
// later - without a presenter noticing.
package cycle

import (
	"context"
	"time"

	"github.com/lmsilva/squire/internal/cross"
	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmfacts"
)

// Snapshot is one pass's whole output. Reports and Queue travel together
// because a finding that crosses them would otherwise pair fresh reports with
// a stale queue, or the reverse.
type Snapshot struct {
	Reports []report.JobReport
	Queue   report.Queue
	// Findings are the allocation findings: what the cards did, drawn from
	// the reports above. ConfigFindings are what jobs asked for, drawn from
	// the whole job list. They stay apart because they are not the same
	// claim - one is about a job in the verdict table, the other can be
	// about a job that is not there at all.
	Findings       []lint.Finding
	ConfigFindings []lint.Finding

	// ClusterUnread says the nodes and partitions could not be read, so the
	// rules that compare a request against the hardware did not run. An
	// unread signal and a measured absence are different facts, and a
	// shorter list with no explanation reads as good news.
	ClusterUnread bool

	// Jobs is every job Slurm returned, not only the ones with cards in the
	// reports above. A presenter never renders this list; it is here because
	// a finding can be about a job the verdict table does not show, and that
	// finding still has to be able to name it.
	Jobs []slurmapi.Job

	// At is when the pass finished, not when a caller asked for it. One
	// snapshot is served to several callers over its cached life, and each
	// needs to know how old the numbers are rather than when it happened to
	// ask for them.
	At time.Time
}

// Builder is what a pass reads from. A consumer-defined interface, so this
// package does not depend on how the join is done and a test needs no
// cluster.
type Builder interface {
	Build(ctx context.Context) (report.Pass, error)
}

// Build runs one pass and derives its findings.
//
// grace is the age below which a job is not judged. It travels into the
// engine so a job that has not yet had the chance to waste anything produces
// no finding - the same ceiling the activity verdict uses, from the same
// thresholds.
func Build(ctx context.Context, b Builder, grace time.Duration) (Snapshot, error) {
	pass, err := b.Build(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	jobs := make([]cross.Job, 0, len(pass.Reports))
	for _, r := range pass.Reports {
		jobs = append(jobs, toCrossJob(r, grace))
	}
	findings := cross.CheckAll(jobs, cross.Queue{
		PendingGPUJobs: pass.Queue.PendingGPUJobs, PendingGPUs: pass.Queue.PendingGPUs,
	})
	return Snapshot{
		Reports: pass.Reports, Queue: pass.Queue, Jobs: pass.Jobs,
		Findings: findings, ConfigFindings: configFindings(pass),
		ClusterUnread: pass.ClusterUnread, At: time.Now(),
	}, nil
}

// configFindings runs the configuration engine over every job in the pass.
//
// It is the same engine squire-lint runs, over the same translation, so the
// page and the command line cannot report different things about one job.
// Without cluster data the spec-only rules still run - the engine takes a nil
// cluster and stays silent about what it cannot compare.
func configFindings(pass report.Pass) []lint.Finding {
	jobs := make([]lint.Job, 0, len(pass.Jobs))
	for _, j := range pass.Jobs {
		jobs = append(jobs, slurmfacts.Job(j))
	}
	var cluster *lint.Cluster
	if !pass.ClusterUnread {
		cluster = slurmfacts.Cluster(pass.Nodes, pass.Partitions)
	}
	return lint.CheckAll(jobs, cluster)
}

// toCrossJob adapts a finished report into the cross-source engine's input.
func toCrossJob(r report.JobReport, grace time.Duration) cross.Job {
	return cross.Job{
		ID: r.Job.JobID, Name: r.Job.Name, User: r.Job.Owner(),
		GPUsRequested: r.GPUs,
		Elapsed:       r.Elapsed,
		Grace:         grace,
		PerGPU:        r.PerGPU,
		HasLit:        r.HasLit, LitGPUs: r.LitGPUs,
		HasFirstWork: r.HasFirstWork, FirstWorkAfter: r.FirstWorkAfter,
	}
}

// Source is where a presenter gets a Snapshot.
type Source interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// SourceFunc adapts a plain function to Source, so a caller that already has
// one does not have to declare a type to carry it.
type SourceFunc func(ctx context.Context) (Snapshot, error)

func (f SourceFunc) Snapshot(ctx context.Context) (Snapshot, error) { return f(ctx) }

// Cache serves the last successful snapshot to any caller arriving within
// minAge of it. Without it every scrape and every page load runs a full
// fan-out across Slurm, Kubernetes and Prometheus, so a caller in a loop
// amplifies into those services rather than into Squire.
//
// sem is a one-slot channel used as a lock, held across the build on purpose:
// a caller arriving mid-build waits, then finds the cache fresh and returns
// without querying anything. One build, however many callers.
//
// It is a channel rather than a sync.Mutex because a mutex cannot be given
// up. A client abandons a request after its own timeout, which is shorter
// than the build budget, and a waiter blocked on a mutex would keep waiting
// for a client that has already gone.
type Cache struct {
	src    Source
	sem    chan struct{}
	minAge time.Duration
	at     time.Time
	last   Snapshot
}

// NewCache is required rather than optional: a nil channel blocks forever, so
// a zero-value Cache would hang the first caller.
func NewCache(src Source, minAge time.Duration) *Cache {
	return &Cache{src: src, sem: make(chan struct{}, 1), minAge: minAge}
}

// Snapshot returns a cached pass if one is younger than minAge, otherwise
// runs a fresh one. Errors are never cached: one transient Slurm hiccup must
// not buy minAge of silence.
//
// Freshness is judged by the timestamp, never by the contents. A pass over an
// idle cluster produces no reports at all, so a "did we get anything" test
// would never cache anything precisely when there is least to gain by
// rebuilding.
func (c *Cache) Snapshot(ctx context.Context) (Snapshot, error) {
	// Taking the slot is the lock. The ctx case is what a mutex cannot do:
	// a caller whose client has given up stops waiting and returns.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	}
	if !c.at.IsZero() && time.Since(c.at) < c.minAge {
		return c.last, nil
	}
	got, err := c.src.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	c.at, c.last = time.Now(), got
	return got, nil
}
