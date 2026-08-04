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
		avg, avgOK, err := b.podScalar(ctx, pods, "avg", clampDur(elapsed))
		if err != nil {
			return nil, err
		}
		peak, peakOK, err := b.podScalar(ctx, pods, "max", b.ZombieWin)
		if err != nil {
			return nil, err
		}
		r := JobReport{
			Job: j, Pods: pods, GPUs: gpus, Elapsed: elapsed,
			AvgUtil: avg, PeakUtil: peak, HasData: avgOK && peakOK,
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

// podScalar aggregates GPU util across a job's pods over a window:
// agg( agg_over_time(DCGM_FI_DEV_GPU_UTIL{pod=~"p1|p2"}[window]) ).
// podScalar aggregates GPU utilization across pods over a window. The
// second return reports whether any telemetry existed.
func (b *Builder) podScalar(ctx context.Context, pods []string, agg string, win time.Duration) (float64, bool, error) {
	if len(pods) == 0 {
		return 0, false, nil
	}
	quoted := make([]string, len(pods))
	for i, p := range pods {
		quoted[i] = regexp.QuoteMeta(p)
	}
	// Anchor the matcher: an unanchored "w-1" would also match "w-10".
	sel := "^(" + strings.Join(quoted, "|") + ")$"
	// The label name is configuration, not a constant: kube-prometheus-stack
	// renames the exporter's "pod" to "exported_pod" (preflight check 4).
	label := b.PodLabel
	if label == "" {
		label = "exported_pod"
	}
	q := fmt.Sprintf(`%s(%s_over_time(DCGM_FI_DEV_GPU_UTIL{%s=~"%s"}[%s]))`,
		agg, agg, label, sel, promDur(win))
	samples, err := b.Prom.Query(ctx, q)
	if err != nil {
		return 0, false, fmt.Errorf("querying gpu util: %w", err)
	}
	if len(samples) == 0 {
		return 0, false, nil
	}
	return samples[0].Value, true, nil
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
