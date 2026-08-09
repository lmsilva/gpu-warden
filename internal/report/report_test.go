package report

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/gpu-warden/internal/promapi"
	"github.com/lmsilva/gpu-warden/internal/slurmapi"
	"github.com/lmsilva/gpu-warden/internal/verdict"
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
type fakeProm struct{ byGPU map[string]gpu }

var selRe = regexp.MustCompile(`exported_pod=~"\^\(([^)]*)\)\$"(?:,gpu=~"\^\(([^)]*)\)\$")?`)

// devices returns the keys a query selects, deduplicated - the capacity query
// names each selector twice (once for FB_USED, once for FB_FREE).
func (f fakeProm) devices(q string) []string {
	seen := map[string]bool{}
	var out []string
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
	keys := f.devices(q)
	if len(keys) == 0 {
		return nil, nil
	}
	// Pick the value each selected device contributes. Order matters: the
	// capacity query names FB_USED and FB_FREE, so it must be matched first.
	var value func(gpu) float64
	switch {
	case strings.Contains(q, "FB_FREE"):
		value = func(g gpu) float64 { return g.fbUsed + g.fbFree }
	case strings.Contains(q, "FB_USED"):
		value = func(g gpu) float64 { return g.fbUsed }
	case strings.Contains(q, "GPU_UTIL"):
		value = func(g gpu) float64 {
			if !strings.HasPrefix(q, "max(") {
				return g.util
			}
			if g.peakUtil == 0 && g.util > 0 {
				return g.util
			}
			return g.peakUtil
		}
	default:
		// Profiling metrics are not scraped in this fake: an empty vector,
		// exactly like a real Prometheus that has never seen the series.
		return nil, nil
	}

	vals := make([]float64, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, value(f.byGPU[k]))
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
