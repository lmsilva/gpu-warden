// Package report joins Slurm jobs, Kubernetes pods, and GPU telemetry
// into per-job efficiency findings.
package report

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lmsilva/gpu-warden/internal/promapi"
	"github.com/lmsilva/gpu-warden/internal/slurmapi"
)

// Consumer-defined interfaces: report asks only for what it needs.
type JobLister interface {
	ListRunningJobs(ctx context.Context) ([]slurmapi.Job, error)
}

type Querier interface {
	Query(ctx context.Context, promql string) ([]promapi.Sample, error)
}

// JobReport is one job's joined truth.
type JobReport struct {
	Job      slurmapi.Job
	Pods     []string
	GPUs     int
	Elapsed  time.Duration
	AvgUtil  float64 // average GPU util (%) across the job's GPUs, since start
	PeakUtil float64 // peak util (%) over the zombie window
	WastedH  float64 // GPU-hours of allocated-but-unused capacity
	HasData  bool    // telemetry actually observed for this job's pods
	Zombie   bool

	// PerGPU records whether telemetry was scoped to the exact GPU devices
	// this job holds. False means the numbers cover every GPU on the job's
	// nodes, which is only equivalent when the job holds them all.
	PerGPU bool
}

// NodeResolver maps Slurm node names to the Kubernetes pod names that DCGM
// labels its metrics with. These are DIFFERENT strings — Slurm says "gpu-0",
// the pod is "slurm-worker-gpu-0" — so the mapping is read from the cluster
// (kube.NodeMap), never derived. Declared here as a consumer-defined interface
// so report stays testable with a two-line fake.
type NodeResolver interface {
	PodNames(ctx context.Context) (map[string]string, error)
}

type Builder struct {
	Jobs      JobLister
	Prom      Querier
	Nodes     NodeResolver
	PodLabel  string // DCGM label carrying the pod name, e.g. "exported_pod"
	ZombiePct float64
	ZombieWin time.Duration
}

// Build produces one JobReport per running GPU job.
func (b *Builder) Build(ctx context.Context) ([]JobReport, error) {
	jobs, err := b.Jobs.ListRunningJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	// Fail with a sentence, not a stack trace: an unattended tool should say
	// what was misconfigured, and a nil NodeResolver panics deep inside Build.
	if b.Nodes == nil {
		return nil, fmt.Errorf("report.Builder.Nodes is nil: a NodeResolver is required")
	}
	// One lookup per cycle, not per job.
	nodeToPod, err := b.Nodes.PodNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving node-to-pod mapping: %w", err)
	}
	var out []JobReport
	for _, j := range jobs {
		gpus := slurmapi.GPUCount(j)
		if gpus == 0 {
			continue // CPU-only jobs are outside warden's mandate
		}
		nodes, err := slurmapi.ExpandNodes(j.Nodes)
		if err != nil {
			return nil, fmt.Errorf("expanding nodes for job %d: %w", j.JobID, err)
		}
		// Translate Slurm node names into pod names. A node absent from the map
		// is skipped rather than fatal: a worker pod can be mid-restart.
		pods := make([]string, 0, len(nodes))
		for _, n := range nodes {
			if p, ok := nodeToPod[n]; ok {
				pods = append(pods, p)
			}
		}
		elapsed := time.Hour // safe default if start_time is unset
		if j.StartTime.Set && j.StartTime.Number > 0 {
			elapsed = time.Since(time.Unix(j.StartTime.Number, 0))
		}
		// Scope telemetry to the exact GPU devices this job holds. On a node
		// shared with another job, reading every GPU on the node would let a
		// neighbour's work exonerate an idle allocation.
		idx := slurmapi.GPUIndices(j)
		targets := b.targets(nodes, nodeToPod, idx)

		avg, avgOK, err := b.jobScalar(ctx, targets, "avg", clampDur(elapsed))
		if err != nil {
			return nil, err
		}
		peak, peakOK, err := b.jobScalar(ctx, targets, "max", b.ZombieWin)
		if err != nil {
			return nil, err
		}
		r := JobReport{
			Job: j, Pods: pods, GPUs: gpus, Elapsed: elapsed,
			AvgUtil: avg, PeakUtil: peak, HasData: avgOK && peakOK,
			PerGPU: idx != nil,
		}
		if r.HasData {
			r.WastedH = float64(gpus) * elapsed.Hours() * (1 - avg/100)
		}
		// A zombie verdict requires positive evidence: telemetry present,
		// a real start time, a full window elapsed, sustained near-zero peak.
		r.Zombie = r.HasData && j.StartTime.Set &&
			elapsed >= b.ZombieWin && peak < b.ZombiePct
		out = append(out, r)
	}
	return out, nil
}

// target is one pod and, when known, the GPU device indices on it that belong
// to the job. An empty Indices means "every GPU on this pod" — correct only
// when the job holds the whole node.
type target struct {
	Pod     string
	Indices []string
}

// targets pairs each of the job's Slurm nodes with its pod name and the GPU
// devices allocated on it. A node with no pod is skipped (worker mid-restart);
// a nil index map degrades every target to pod-wide scoping rather than
// dropping the job.
func (b *Builder) targets(nodes []string, nodeToPod map[string]string, idx map[string][]string) []target {
	out := make([]target, 0, len(nodes))
	for _, n := range nodes {
		pod, ok := nodeToPod[n]
		if !ok {
			continue
		}
		out = append(out, target{Pod: pod, Indices: idx[n]})
	}
	return out
}

// jobScalar aggregates GPU utilization across exactly the GPUs a job holds,
// over a window:
//
//	agg( agg_over_time(UTIL{pod="p1",gpu=~"^(0|1)$"}[win])
//	  or agg_over_time(UTIL{pod="p2",gpu=~"^(3)$"}[win]) )
//
// One selector per pod, unioned with PromQL's "or" set operator, because the
// device set differs per node: a single {pod=~"p1|p2", gpu=~"0|1|3"} matcher
// would form the cross product and pull in GPU 3 on p1, which belongs to
// somebody else. The second return reports whether any telemetry existed.
func (b *Builder) jobScalar(ctx context.Context, targets []target, agg string, win time.Duration) (float64, bool, error) {
	if len(targets) == 0 {
		return 0, false, nil
	}
	// The label name is configuration, not a constant: kube-prometheus-stack
	// renames the exporter's "pod" to "exported_pod" (preflight check 4).
	label := b.PodLabel
	if label == "" {
		label = "exported_pod"
	}
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf(`%s_over_time(DCGM_FI_DEV_GPU_UTIL{%s}[%s])`,
			agg, selector(label, t), promDur(win)))
	}
	q := fmt.Sprintf("%s(%s)", agg, strings.Join(parts, " or "))
	samples, err := b.Prom.Query(ctx, q)
	if err != nil {
		return 0, false, fmt.Errorf("querying gpu util: %w", err)
	}
	if len(samples) == 0 {
		return 0, false, nil
	}
	return samples[0].Value, true, nil
}

// selector renders the label matchers for one target. Both matchers are
// anchored: an unanchored "w-1" would also match "w-10", and an unanchored
// "1" would also match GPU 11.
func selector(label string, t target) string {
	sel := fmt.Sprintf(`%s=~"^(%s)$"`, label, regexp.QuoteMeta(t.Pod))
	if len(t.Indices) == 0 {
		return sel
	}
	quoted := make([]string, len(t.Indices))
	for i, ix := range t.Indices {
		quoted[i] = regexp.QuoteMeta(ix)
	}
	return fmt.Sprintf(`%s,gpu=~"^(%s)$"`, sel, strings.Join(quoted, "|"))
}

func clampDur(d time.Duration) time.Duration {
	if d < time.Minute {
		return time.Minute
	}
	if d > 24*time.Hour {
		return 24 * time.Hour
	}
	return d
}

func promDur(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Seconds()))
}