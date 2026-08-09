// Package report joins Slurm jobs, Kubernetes pods, and GPU telemetry
// into per-job efficiency findings.
package report

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lmsilva/squire/internal/promapi"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
)

// DCGM field names Squire reads. Named constants because they appear in
// several queries and a typo does not fail: Prometheus answers an unknown
// metric name with an empty vector and HTTP 200, so the mistake looks exactly
// like "this cluster does not scrape that field".
const (
	metricUtil   = "DCGM_FI_DEV_GPU_UTIL"            // percent, 0-100
	metricFBUsed = "DCGM_FI_DEV_FB_USED"             // framebuffer used, MiB
	metricFBFree = "DCGM_FI_DEV_FB_FREE"             // framebuffer free, MiB
	metricGrEng  = "DCGM_FI_PROF_GR_ENGINE_ACTIVE"   // engine active, 0-1
	metricSM     = "DCGM_FI_PROF_SM_ACTIVE"          // fraction of SMs active, 0-1
	metricTensor = "DCGM_FI_PROF_PIPE_TENSOR_ACTIVE" // tensor pipe active, 0-1
	metricPower  = "DCGM_FI_DEV_POWER_USAGE"         // watts
)

// Bars for the instrument-fault guard. Engine this flat on every device in the
// cluster, while some device somewhere is this busy, is a broken sensor rather
// than an idle fleet.
const (
	faultEngineFrac = 0.05
	faultUtilPct    = 50
)

// Consumer-defined interfaces: report asks only for what it needs.
type JobLister interface {
	ListRunningJobs(ctx context.Context) ([]slurmapi.Job, error)
}

type Querier interface {
	Query(ctx context.Context, promql string) ([]promapi.Sample, error)
}

// JobReport is one job's joined truth: the Slurm facts, the telemetry Squire
// measured, and the verdict the judge returned for them.
type JobReport struct {
	Job     slurmapi.Job
	Pods    []string
	GPUs    int
	Elapsed time.Duration

	AvgUtil  float64 // average GPU util (%) across the job's GPUs, since start
	PeakUtil float64 // peak util (%) over the idle window

	PeakMemMiB  float64 // peak framebuffer used on a single GPU, MiB
	CapacityMiB float64 // that GPU's total framebuffer, MiB (used + free)
	PeakMemFrac float64 // PeakMemMiB / CapacityMiB, 0-1

	WastedH float64 // GPU-hours of allocated-but-unused capacity
	HasData bool    // utilization telemetry actually observed for this job's pods

	// EngineFaulted records that the engine signal was dropped cluster-wide
	// this cycle because it looked broken. Set on every report in the cycle,
	// since the judgement is fleet-level.
	EngineFaulted bool

	// PerGPU records whether telemetry was scoped to the exact GPU devices
	// this job holds. False means the numbers cover every GPU on the job's
	// nodes, which is only equivalent when the job holds them all.
	PerGPU bool

	// Verdict is the two-axis judgement. Its zero value reads as
	// "analyzing / unknown", so a report built before judging never looks
	// like an all-clear.
	Verdict verdict.Verdict
}

// IsZombie is the one-line question the Event path and the exporter ask. It
// replaces the old boolean field: the judgement lives in one place now, and
// every caller derives from it rather than keeping a second, drift-prone copy.
func (r JobReport) IsZombie() bool { return r.Verdict.Activity == verdict.Zombie }

// NodeResolver maps Slurm node names to the Kubernetes pod names that DCGM
// labels its metrics with. These are DIFFERENT strings — Slurm says "gpu-0",
// the pod is "slurm-worker-gpu-0" — so the mapping is read from the cluster
// (kube.NodeMap), never derived. Declared here as a consumer-defined interface
// so report stays testable with a two-line fake.
type NodeResolver interface {
	PodNames(ctx context.Context) (map[string]string, error)
}

type Builder struct {
	Jobs     JobLister
	Prom     Querier
	Nodes    NodeResolver
	PodLabel string // DCGM label carrying the pod name, e.g. "exported_pod"

	// Thresholds are the operator's globally-overridden knobs. A zero value
	// means "use Squire's shipped defaults" - otherwise a caller that forgot
	// to set them would get a judge with a 0s idle window, which would flag
	// everything.
	Thresholds verdict.Thresholds
}

// thresholds returns the configured knobs, falling back to the shipped
// defaults when the Builder was constructed without them - otherwise a caller
// that forgot would get a judge with a 0s idle window, which flags every job.
func (b *Builder) thresholds() verdict.Thresholds {
	if b.Thresholds.IdleWindow == 0 {
		return verdict.DefaultThresholds()
	}
	return b.Thresholds
}

// podLabel is configuration, not a constant: kube-prometheus-stack renames the
// exporter's "pod" label to "exported_pod" when it scrapes, because the target
// label and the scraped label collide.
func (b *Builder) podLabel() string {
	if b.PodLabel == "" {
		return "exported_pod"
	}
	return b.PodLabel
}

// Build produces one JobReport per running GPU job.
func (b *Builder) Build(ctx context.Context) ([]JobReport, error) {
	th := b.thresholds()
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
	// One fleet-level check per cycle, before any job is judged.
	faulted, err := b.engineFaulted(ctx)
	if err != nil {
		return nil, fmt.Errorf("checking engine signal health: %w", err)
	}
	var out []JobReport
	for _, j := range jobs {
		gpus := slurmapi.GPUCount(j)
		if gpus == 0 {
			continue // CPU-only jobs are outside Squire's mandate
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
		// Elapsed drives both the whole-run query window and the judge's
		// warmup arithmetic. Without a real start_time Squire cannot age the
		// job, and the judge is TOLD so (HasStart) rather than being handed a
		// plausible-looking guess.
		hasStart := j.StartTime.Set && j.StartTime.Number > 0
		elapsed := time.Hour // safe default query window if start_time is unset
		if hasStart {
			elapsed = time.Since(time.Unix(j.StartTime.Number, 0))
		}
		// Scope telemetry to the exact GPU devices this job holds. On a node
		// shared with another job, reading every GPU on the node would let a
		// neighbour's work exonerate an idle allocation.
		idx := slurmapi.GPUIndices(j)
		targets := b.targets(nodes, nodeToPod, idx)

		m, err := b.collect(ctx, targets, elapsed, hasStart, th, !faulted)
		if err != nil {
			return nil, fmt.Errorf("collecting telemetry for job %d: %w", j.JobID, err)
		}

		r := JobReport{
			Job: j, Pods: pods, GPUs: gpus, Elapsed: elapsed,
			AvgUtil: m.AvgUtil, PeakUtil: m.PeakUtil,
			PeakMemMiB: m.PeakMemMiB, CapacityMiB: m.CapacityMiB,
			PeakMemFrac:   m.PeakMemFrac,
			HasData:       m.HasUtil,
			PerGPU:        idx != nil,
			EngineFaulted: faulted,
		}
		if r.HasData {
			r.WastedH = float64(gpus) * elapsed.Hours() * (1 - m.AvgUtil/100)
		}
		// Every "is this waste" decision happens in one pure function.
		// report's job is to measure honestly and hand the numbers over.
		r.Verdict = verdict.Judge(m, th)
		out = append(out, r)
	}
	return out, nil
}

// target is one pod and, when known, the GPU device indices on it that belong
// to the job. An empty Indices means "every GPU on this pod" - correct only
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

// engineFaulted reports whether the engine signal looks broken rather than the
// fleet looking idle: a dcgm-exporter defect makes GR_ENGINE_ACTIVE read 0 on
// every GPU while GPU_UTIL reads ~100%. A missing series is harmless; a
// genuine-looking zero would corroborate every idle reading at once. If either
// query returns nothing the guard stays off.
func (b *Builder) engineFaulted(ctx context.Context) (bool, error) {
	eng, engOK, err := b.scalar(ctx, fmt.Sprintf("max(%s)", metricGrEng))
	if err != nil || !engOK {
		return false, err
	}
	util, utilOK, err := b.scalar(ctx, fmt.Sprintf("max(%s)", metricUtil))
	if err != nil || !utilOK {
		return false, err
	}
	return eng < faultEngineFrac && util >= faultUtilPct, nil
}

// collect gathers every DCGM signal for one job's devices and packs them into
// the judge's input struct. Each signal is independent: a missing profiling
// metric leaves its Has* flag false and costs the verdict some confidence,
// but never fails the cycle.
func (b *Builder) collect(ctx context.Context, targets []target, elapsed time.Duration, hasStart bool, th verdict.Thresholds, engineOK bool) (verdict.Metrics, error) {
	m := verdict.Metrics{Elapsed: elapsed, HasStart: hasStart}
	if len(targets) == 0 {
		return m, nil // no pods resolved: judged as "no telemetry", never as ok
	}
	life := clampDur(elapsed) // the whole run so far
	// The trailing window is clamped to the job's own age. Without this, a
	// job younger than the window reads whatever ran on its devices before it
	// started - an idle job inherits the previous job's utilization and comes
	// out healthy. clampDur then floors it so a seconds-old job still gets a
	// query Prometheus can answer.
	win := clampDur(verdict.PeakWindow(elapsed, th.IdleWindow))

	// Utilization: the activity signal. Average over the job's life answers
	// "did this ever work"; peak over the trailing window answers "is it
	// working now". Peak rather than average on the window is deliberate -
	// one honest spike clears a job of being idle.
	avgUtil, avgOK, err := b.scalar(ctx, b.windowed("avg", metricUtil, targets, life))
	if err != nil {
		return m, err
	}
	peakUtil, peakOK, err := b.scalar(ctx, b.windowed("max", metricUtil, targets, win))
	if err != nil {
		return m, err
	}
	m.AvgUtil, m.PeakUtil, m.HasUtil = avgUtil, peakUtil, avgOK && peakOK

	// Framebuffer memory: the sizing signal. DCGM reports memory in absolute
	// MiB, so a raw number means nothing without the card's size. Capacity
	// comes from used+free on the same series, which is how Squire learns the
	// card size without being told what hardware it is on.
	peakMem, peakMemOK, err := b.scalar(ctx, b.windowed("max", metricFBUsed, targets, life))
	if err != nil {
		return m, err
	}
	avgMem, avgMemOK, err := b.scalar(ctx, b.windowed("avg", metricFBUsed, targets, life))
	if err != nil {
		return m, err
	}
	capacity, capOK, err := b.scalar(ctx, b.capacityQuery(targets))
	if err != nil {
		return m, err
	}
	if peakMemOK && avgMemOK && capOK && capacity > 0 {
		m.HasMem = true
		m.PeakMemMiB, m.CapacityMiB = peakMem, capacity
		m.PeakMemFrac = peakMem / capacity
		m.AvgMemFrac = avgMem / capacity
	}

	// Enrichment signals: refine confidence, never block a verdict. These are
	// DCGM profiling fields. Plenty of installs do not scrape them, so an
	// empty result is normal and simply leaves the flag false.
	if engineOK {
		if gr, ok, err := b.scalar(ctx, b.windowed("max", metricGrEng, targets, win)); err != nil {
			return m, err
		} else if ok {
			m.HasGrEngine, m.PeakGrEngine = true, gr
		}
	}
	if sm, ok, err := b.scalar(ctx, b.windowed("max", metricSM, targets, win)); err != nil {
		return m, err
	} else if ok {
		m.HasSM, m.PeakSM = true, sm
	}
	if tc, ok, err := b.scalar(ctx, b.windowed("max", metricTensor, targets, win)); err != nil {
		return m, err
	} else if ok {
		m.HasTensor, m.PeakTensor = true, tc
	}
	if pw, ok, err := b.scalar(ctx, b.windowed("max", metricPower, targets, win)); err != nil {
		return m, err
	} else if ok {
		m.HasPower, m.PeakPowerW = true, pw
	}
	return m, nil
}

// windowed builds one query aggregating a metric over a window across exactly
// the GPUs a job holds:
//
//	agg( agg_over_time(METRIC{pod="p1",gpu=~"^(0|1)$"}[win])
//	  or agg_over_time(METRIC{pod="p2",gpu=~"^(3)$"}[win]) )
//
// One selector per pod, unioned with PromQL's "or", because the device set
// differs per node: a single {pod=~"p1|p2",gpu=~"0|1|3"} matcher would form
// the cross product and pull in GPU 3 on p1, which belongs to somebody else.
func (b *Builder) windowed(agg, metric string, targets []target, win time.Duration) string {
	label := b.podLabel()
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		parts = append(parts, fmt.Sprintf("%s_over_time(%s{%s}[%s])",
			agg, metric, selector(label, t), promDur(win)))
	}
	return fmt.Sprintf("%s(%s)", agg, strings.Join(parts, " or "))
}

// capacityQuery is the one query with no window. Prometheus adds FB_USED and
// FB_FREE element-by-element - they carry identical labels - giving per-GPU
// total memory, and max picks the card. Same "or" union, so it stays scoped
// to the job's own devices.
func (b *Builder) capacityQuery(targets []target) string {
	label := b.podLabel()
	parts := make([]string, 0, len(targets))
	for _, t := range targets {
		sel := selector(label, t)
		parts = append(parts, fmt.Sprintf("(%s{%s} + %s{%s})",
			metricFBUsed, sel, metricFBFree, sel))
	}
	return fmt.Sprintf("max(%s)", strings.Join(parts, " or "))
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

// scalar runs one PromQL query and returns its single value. The second return
// reports whether any series existed at all - the difference between "measured
// zero" and "never measured", which Squire must never confuse.
func (b *Builder) scalar(ctx context.Context, promql string) (float64, bool, error) {
	samples, err := b.Prom.Query(ctx, promql)
	if err != nil {
		return 0, false, fmt.Errorf("querying %q: %w", promql, err)
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
