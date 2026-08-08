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

// fakeProm answers per (pod, gpu index) so a test can model a node shared by
// two jobs. Keys are "pod/gpu"; a query is answered by averaging (or maxing)
// the devices its selectors actually name, which is what a real Prometheus
// does with the "or" union.
type fakeProm struct{ byGPU map[string]float64 }

var selRe = regexp.MustCompile(`exported_pod=~"\^\(([^)]*)\)\$"(?:,gpu=~"\^\(([^)]*)\)\$")?`)

func (f fakeProm) Query(ctx context.Context, q string) ([]promapi.Sample, error) {
	var vals []float64
	for _, m := range selRe.FindAllStringSubmatch(q, -1) {
		pod, idx := m[1], m[2]
		for key, v := range f.byGPU {
			p, g, _ := strings.Cut(key, "/")
			if p != pod {
				continue
			}
			// No gpu matcher means the query selects every GPU on the pod —
			// which is exactly the bug this test exists to catch.
			if idx == "" || slices.Contains(strings.Split(idx, "|"), g) {
				vals = append(vals, v)
			}
		}
	}
	if len(vals) == 0 {
		return nil, nil
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
	start := time.Now().Add(-2 * time.Hour).Unix()
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
		// THE REGRESSION CASE. Jobs 5 and 6 share one 4-GPU node. Job 5 holds
		// GPUs 0-1 and is dead idle; job 6 holds GPUs 2-3 and is saturated.
		// With pod-only scoping both read ~97% and the zombie walks free.
		{JobID: 5, Name: "sharer-idle", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-3", TresAlloc: "cpu=4,gres/gpu=2",
			GresDetail: []string{"gpu:2(IDX:0-1)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 6, Name: "sharer-busy", UserName: "ana", State: []string{"RUNNING"},
			Nodes: "w-3", TresAlloc: "cpu=4,gres/gpu=2",
			GresDetail: []string{"gpu:2(IDX:2-3)"},
			StartTime:  slurmapi.NoVal{Set: true, Number: start}},
	}}
	prom := fakeProm{byGPU: map[string]float64{
		"pod-w-0/0": 90,
		"pod-w-1/0": 0,
		"pod-w-3/0": 0, "pod-w-3/1": 0, // job 5's devices: idle
		"pod-w-3/2": 95, "pod-w-3/3": 97, // job 6's devices: busy
	}}

	nodes := fakeNodes{"w-0": "pod-w-0", "w-1": "pod-w-1", "w-2": "pod-w-2",
		"w-3": "pod-w-3", "w-9": "pod-w-9"}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: nodes, PodLabel: "exported_pod",
		ZombiePct: 5, ZombieWin: 15 * time.Minute}
	got, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("want 5 GPU jobs, got %d", len(got))
	}
	if got[0].Zombie || !got[1].Zombie {
		t.Errorf("zombie judgment wrong: %+v", got)
	}
	if got[1].WastedH < 1.9 || got[1].WastedH > 2.1 {
		t.Errorf("zombie wasted ≈2 GPU-hours, got %.2f", got[1].WastedH)
	}
	// Jobs without telemetry must never receive a verdict.
	if got[2].HasData || got[2].Zombie {
		t.Errorf("no-telemetry job must be HasData=false and never zombie: %+v", got[2])
	}

	// The shared node. Before per-GPU scoping, the idle job read its
	// neighbour's utilization and was never flagged.
	idle, busy := got[3], got[4]
	if idle.Job.Name != "sharer-idle" || busy.Job.Name != "sharer-busy" {
		t.Fatalf("unexpected order: %s, %s", idle.Job.Name, busy.Job.Name)
	}
	if idle.PeakUtil != 0 {
		t.Errorf("idle sharer read %.0f%% — telemetry is leaking from the neighbour's GPUs", idle.PeakUtil)
	}
	if !idle.Zombie {
		t.Errorf("idle sharer must be flagged: %+v", idle)
	}
	if busy.PeakUtil != 97 || busy.Zombie {
		t.Errorf("busy sharer misjudged: peak %.0f%%, zombie=%v", busy.PeakUtil, busy.Zombie)
	}
	if !idle.PerGPU || !busy.PerGPU {
		t.Errorf("both sharers had gres_detail, so attribution must be per-GPU")
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
