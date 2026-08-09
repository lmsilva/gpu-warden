package report

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/promapi"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
)

type fakeJobs struct{ jobs []slurmapi.Job }

func (f fakeJobs) ListRunningJobs(ctx context.Context) ([]slurmapi.Job, error) {
	return f.jobs, nil
}

// fakeNodes stands in for kube.Client. Deliberately NOT an identity map:
// mapping "w-0" to "pod-w-0" means a regression that passes Slurm node names
// straight through to Prometheus fails here instead of silently returning
// no-data against a live cluster.
type fakeNodes map[string]string

func (f fakeNodes) PodNames(ctx context.Context) (map[string]string, error) {
	return f, nil
}

// gpu is one fake device's telemetry, in DCGM's own units: utilization in
// percent, framebuffer in MiB. peakUtil is set only when it differs from util,
// which is how the crash signature is modelled.
type gpu struct {
	util     float64
	peakUtil float64
	fbUsed   float64
	fbFree   float64
}

// fakeProm answers per (metric, pod, gpu index), so a test can model both a
// node shared by two jobs and a query that asks the wrong question. Keys are
// "pod/gpu". Queries are answered by aggregating only the devices their
// selectors actually name, which is what a real Prometheus does with "or".
type fakeProm struct {
	byGPU map[string]gpu
	// engine holds GR_ENGINE_ACTIVE per device. Nil means the cluster does
	// not scrape it at all, which is the common case and what most of these
	// tests model.
	engine map[string]float64
	// seen records every query, so a test can assert on the range a query
	// covers rather than only on the number that comes back. A pointer
	// because Query has a value receiver.
	seen *[]string
}

var selRe = regexp.MustCompile(`exported_pod=~"\^\(([^)]*)\)\$"(?:,gpu=~"\^\(([^)]*)\)\$")?`)

// devices returns the keys a query selects, deduplicated - the capacity query
// names each selector twice (once for FB_USED, once for FB_FREE).
func (f fakeProm) devices(q string) []string {
	seen := map[string]bool{}
	var out []string
	// A query with no selector at all is fleet-wide. The instrument-fault
	// guard asks two of those, and they must see every device or the guard
	// can never fire.
	if !selRe.MatchString(q) {
		for key := range f.byGPU {
			out = append(out, key)
		}
		slices.Sort(out)
		return out
	}
	for _, m := range selRe.FindAllStringSubmatch(q, -1) {
		pod, idx := m[1], m[2]
		for key := range f.byGPU {
			p, g, _ := strings.Cut(key, "/")
			if p != pod || seen[key] {
				continue
			}
			// No gpu matcher means the query selects every GPU on the pod -
			// which is exactly the bug this test exists to catch.
			if idx == "" || slices.Contains(strings.Split(idx, "|"), g) {
				seen[key] = true
				out = append(out, key)
			}
		}
	}
	return out
}

func (f fakeProm) Query(ctx context.Context, q string) ([]promapi.Sample, error) {
	if f.seen != nil {
		*f.seen = append(*f.seen, q)
	}
	keys := f.devices(q)
	if len(keys) == 0 {
		return nil, nil
	}
	// Pick the value each selected device contributes. Keyed by device rather
	// than by gpu struct, because the engine reading lives in its own map.
	// Order matters: the capacity query names FB_USED and FB_FREE, so it must
	// be matched first.
	var value func(key string) float64
	switch {
	case strings.Contains(q, "FB_FREE"):
		value = func(k string) float64 { return f.byGPU[k].fbUsed + f.byGPU[k].fbFree }
	case strings.Contains(q, "FB_USED"):
		value = func(k string) float64 { return f.byGPU[k].fbUsed }
	case strings.Contains(q, "GPU_UTIL"):
		value = func(k string) float64 {
			g := f.byGPU[k]
			if !strings.HasPrefix(q, "max(") {
				return g.util
			}
			if g.peakUtil == 0 && g.util > 0 {
				return g.util
			}
			return g.peakUtil
		}
	case strings.Contains(q, "GR_ENGINE_ACTIVE"):
		if f.engine == nil {
			return nil, nil // this cluster does not scrape it
		}
		value = func(k string) float64 { return f.engine[k] }
	default:
		// The other profiling metrics are not scraped in this fake: an empty
		// vector, exactly like a real Prometheus that has never seen the
		// series.
		return nil, nil
	}

	vals := make([]float64, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, value(k))
	}
	out := vals[0]
	if strings.HasPrefix(q, "max(") {
		for _, v := range vals {
			if v > out {
				out = v
			}
		}
	} else {
		sum := 0.0
		for _, v := range vals {
			sum += v
		}
		out = sum / float64(len(vals))
	}
	return []promapi.Sample{{Labels: map[string]string{}, Value: out}}, nil
}

func TestBuild(t *testing.T) {
	const GiB = 1024.0 // DCGM reports framebuffer in MiB
	start := time.Now().Add(-3 * time.Hour).Unix()
	jobs := fakeJobs{jobs: []slurmapi.Job{
		{JobID: 1, Name: "tinygpt", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-0", TresAlloc: "cpu=4,gres/gpu=1",
			GresDetail: []string{"gpu:1(IDX:0)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 2, Name: "zombie", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-1", TresAlloc: "cpu=1,gres/gpu=1",
			GresDetail: []string{"gpu:1(IDX:0)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 3, Name: "cpu-only", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-2", TresAlloc: "cpu=8"},
		{JobID: 4, Name: "no-telemetry", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-9", TresAlloc: "cpu=1,gres/gpu=1", // no fakeProm entry
			GresDetail: []string{"gpu:1(IDX:0)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 5, Name: "sharer-idle", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-3", TresAlloc: "cpu=4,gres/gpu=2",
			GresDetail: []string{"gpu:2(IDX:0-1)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 6, Name: "sharer-busy", UserName: "ana", State: []string{"RUNNING"},
			Nodes: "w-3", TresAlloc: "cpu=4,gres/gpu=2",
			GresDetail: []string{"gpu:2(IDX:2-3)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 7, Name: "oversized", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-4", TresAlloc: "cpu=4,gres/gpu=1",
			GresDetail: []string{"gpu:1(IDX:0)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
	}}
	prom := fakeProm{byGPU: map[string]gpu{
		"pod-w-0/0": {util: 90, fbUsed: 10 * GiB, fbFree: 5 * GiB},
		"pod-w-1/0": {util: 0, fbUsed: 3 * GiB, fbFree: 12 * GiB},
		"pod-w-3/0": {util: 0, fbUsed: 1 * GiB, fbFree: 14 * GiB}, // job 5: idle
		"pod-w-3/1": {util: 0, fbUsed: 1 * GiB, fbFree: 14 * GiB},
		"pod-w-3/2": {util: 95, fbUsed: 11 * GiB, fbFree: 4 * GiB}, // job 6: busy
		"pod-w-3/3": {util: 97, fbUsed: 11 * GiB, fbFree: 4 * GiB},
		"pod-w-4/0": {util: 45, fbUsed: 2 * GiB, fbFree: 13 * GiB},
	}}

	nodes := fakeNodes{"w-0": "pod-w-0", "w-1": "pod-w-1", "w-2": "pod-w-2",
		"w-3": "pod-w-3", "w-4": "pod-w-4", "w-9": "pod-w-9"}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: nodes, PodLabel: "exported_pod",
		Thresholds: verdict.DefaultThresholds()}
	got, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("want 6 GPU jobs (the CPU-only job is filtered), got %d", len(got))
	}

	want := []struct {
		name     string
		activity verdict.Activity
		sizing   verdict.Sizing
	}{
		{"tinygpt", verdict.Healthy, verdict.RightSized},
		{"zombie", verdict.Zombie, verdict.SizingUnknown},
		{"no-telemetry", verdict.Analyzing, verdict.SizingUnknown},
		{"sharer-idle", verdict.Zombie, verdict.SizingUnknown},
		{"sharer-busy", verdict.Healthy, verdict.RightSized},
		{"oversized", verdict.Healthy, verdict.OverProvisioned},
	}
	for i, w := range want {
		v := got[i].Verdict
		if got[i].Job.Name != w.name {
			t.Fatalf("report %d is %q, expected %q", i, got[i].Job.Name, w.name)
		}
		if v.Activity != w.activity || v.Sizing != w.sizing {
			t.Errorf("%s: got %s, want %s / %s", w.name, v.Summary(), w.activity, w.sizing)
		}
		t.Logf("%-13s -> %-34s :: %s", w.name, v.Summary(), v.Reason())
	}

	// The zombie held one idle GPU for three hours.
	if got[1].WastedH < 2.9 || got[1].WastedH > 3.1 {
		t.Errorf("zombie wasted ≈3 GPU-hours, got %.2f", got[1].WastedH)
	}
	// A job with no telemetry must never receive an all-clear.
	if got[2].HasData || got[2].IsZombie() {
		t.Errorf("no-telemetry job must be HasData=false and never zombie: %+v", got[2])
	}
	// Memory must be carried through as MiB plus a fraction of the card, and
	// the card size must be derived rather than assumed.
	if got[5].PeakMemMiB != 2*GiB || got[5].CapacityMiB != 15*GiB {
		t.Errorf("oversized job memory wrong: %.0f/%.0f MiB",
			got[5].PeakMemMiB, got[5].CapacityMiB)
	}

	// The shared node. Before per-GPU scoping, the idle job read its
	// neighbour's utilization and was never flagged.
	idle, busy := got[3], got[4]
	if idle.PeakUtil != 0 {
		t.Errorf("idle sharer read %.0f%% - telemetry is leaking from the neighbour's GPUs", idle.PeakUtil)
	}
	if busy.PeakUtil != 97 {
		t.Errorf("busy sharer read %.0f%%, want 97", busy.PeakUtil)
	}
	if !idle.PerGPU || !busy.PerGPU {
		t.Errorf("both sharers had gres_detail, so attribution must be per-GPU")
	}
	// Memory is scoped per device too, not just utilization: the idle job
	// must not inherit its neighbour's 11 GiB footprint.
	if idle.PeakMemMiB != 1*GiB {
		t.Errorf("idle sharer peak memory %.0f MiB - memory is leaking too", idle.PeakMemMiB)
	}
}

// engineJob is the one-job fixture the two guard tests share: a single GPU on
// one node, running long enough to be past warmup.
func engineJob() (fakeJobs, fakeNodes) {
	start := time.Now().Add(-3 * time.Hour).Unix()
	return fakeJobs{jobs: []slurmapi.Job{
			{JobID: 1, Name: "busy", UserName: "luis", State: []string{"RUNNING"},
				Nodes: "w-0", TresAlloc: "cpu=4,gres/gpu=1",
				GresDetail: []string{"gpu:1(IDX:0)"},
				StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		}},
		fakeNodes{"w-0": "pod-w-0"}
}

// TestEngineFaultGuard: the engine reads flat while a GPU is demonstrably
// busy, so the instrument is faulted rather than the fleet being idle. The
// signal is dropped for the cycle, the report says so, and the verdict is
// judged on utilization alone - NOT downgraded on a reading we do not believe.
func TestEngineFaultGuard(t *testing.T) {
	const GiB = 1024.0
	jobs, nodes := engineJob()
	prom := fakeProm{
		byGPU:  map[string]gpu{"pod-w-0/0": {util: 97, fbUsed: 10 * GiB, fbFree: 5 * GiB}},
		engine: map[string]float64{"pod-w-0/0": 0}, // scraped, and flat
	}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: nodes, PodLabel: "exported_pod",
		Thresholds: verdict.DefaultThresholds()}
	got, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 report, got %d", len(got))
	}
	r := got[0]
	if !r.EngineFaulted {
		t.Errorf("engine flat everywhere while a GPU reads 97%% is a faulted sensor, not an idle fleet")
	}
	if r.Verdict.Activity != verdict.Healthy {
		t.Errorf("a busy job stays healthy whatever the engine says: %s", r.Verdict.Summary())
	}
	// The point of the guard: a signal we have decided not to believe must not
	// be allowed to lower confidence either.
	if r.Verdict.Confidence != verdict.ConfHigh {
		t.Errorf("dropped signal must not downgrade the verdict, got %s", r.Verdict.Confidence)
	}
	t.Logf("%s :: %s", r.Verdict.Summary(), r.Verdict.Reason())
}

// TestEngineUnlitIsBelieved is the other half. Same flat engine reading, but
// nothing on the fleet is working hard, so there is no contradiction and no
// reason to distrust the sensor. The signal is used, and it drops confidence.
func TestEngineUnlitIsBelieved(t *testing.T) {
	const GiB = 1024.0
	jobs, nodes := engineJob()
	prom := fakeProm{
		byGPU:  map[string]gpu{"pod-w-0/0": {util: 40, fbUsed: 10 * GiB, fbFree: 5 * GiB}},
		engine: map[string]float64{"pod-w-0/0": 0.02},
	}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: nodes, PodLabel: "exported_pod",
		Thresholds: verdict.DefaultThresholds()}
	got, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := got[0]
	if r.EngineFaulted {
		t.Errorf("no GPU is busy, so a quiet engine is not evidence of a broken sensor")
	}
	if r.Verdict.Activity != verdict.Healthy {
		t.Errorf("an optional signal must never change the finding: %s", r.Verdict.Summary())
	}
	if r.Verdict.Confidence != verdict.ConfLow {
		t.Errorf("an uncorroborated utilization reading is low confidence, got %s", r.Verdict.Confidence)
	}
	t.Logf("%s :: %s", r.Verdict.Summary(), r.Verdict.Reason())
}

// TestTargetsFallback proves the degradation path: with no gres_detail, the
// query scopes by pod only and the report says so, rather than dropping the
// job or inventing an index set.
func TestTargetsFallback(t *testing.T) {
	b := &Builder{PodLabel: "exported_pod"}
	tg := b.targets([]string{"w-0"}, map[string]string{"w-0": "pod-w-0"}, nil)
	if len(tg) != 1 || len(tg[0].Indices) != 0 {
		t.Fatalf("expected one pod-wide target, got %+v", tg)
	}
	if s := selector("exported_pod", tg[0]); strings.Contains(s, "gpu=~") {
		t.Errorf("fallback selector must not constrain gpu: %s", s)
	}
	tg = b.targets([]string{"w-0"}, map[string]string{"w-0": "pod-w-0"},
		map[string][]string{"w-0": {"0", "2"}})
	if s := selector("exported_pod", tg[0]); s != `exported_pod=~"^(pod-w-0)$",gpu=~"^(0|2)$"` {
		t.Errorf("unexpected selector: %s", s)
	}
}

// TestQueryShapes pins the two query builders. They are the only place the
// per-device "or" union is expressed, and a regression here is invisible in
// the other tests - a cross-product matcher still returns a number.
func TestQueryShapes(t *testing.T) {
	b := &Builder{PodLabel: "exported_pod"}
	tg := []target{
		{Pod: "pod-a", Indices: []string{"0", "1"}},
		{Pod: "pod-b", Indices: []string{"3"}},
	}
	q := b.windowed("max", metricUtil, tg, 30*time.Minute)
	if strings.Count(q, " or ") != 1 {
		t.Errorf("one union per extra pod expected: %s", q)
	}
	if !strings.Contains(q, `gpu=~"^(0|1)$"`) || !strings.Contains(q, `gpu=~"^(3)$"`) {
		t.Errorf("each pod must carry its own device set: %s", q)
	}
	c := b.capacityQuery(tg)
	if !strings.Contains(c, metricFBUsed) || !strings.Contains(c, metricFBFree) {
		t.Errorf("capacity is used+free: %s", c)
	}
	if strings.Contains(c, "_over_time") {
		t.Errorf("capacity is an instant query, not a windowed one: %s", c)
	}
}

// TestWindowClampedToJobAge is the regression test for a job reading its
// predecessor's telemetry. A 10-minute job with the default 30-minute idle
// window used to query 30 minutes back, 20 of them before the job existed, so
// an idle allocation on a recently-busy device came out healthy.
func TestWindowClampedToJobAge(t *testing.T) {
	const GiB = 1024.0
	var seen []string
	start := time.Now().Add(-10 * time.Minute).Unix()
	jobs := fakeJobs{jobs: []slurmapi.Job{
		{JobID: 1, Name: "young", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-0", TresAlloc: "cpu=4,gres/gpu=1",
			GresDetail: []string{"gpu:1(IDX:0)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
	}}
	prom := fakeProm{
		byGPU: map[string]gpu{"pod-w-0/0": {util: 0, fbUsed: 0, fbFree: 15 * GiB}},
		seen:  &seen,
	}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: fakeNodes{"w-0": "pod-w-0"},
		PodLabel: "exported_pod", Thresholds: verdict.DefaultThresholds()}
	if _, err := b.Build(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no queries recorded")
	}
	// The default idle window is 30m. Nothing may ask for it on a 10m job.
	for _, q := range seen {
		if strings.Contains(q, "[1800s]") {
			t.Errorf("query reaches back past the job's start: %s", q)
		}
	}
}
