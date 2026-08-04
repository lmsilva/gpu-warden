package report

import (
	"context"
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

type fakeProm struct{ byPod map[string]float64 }

func (f fakeProm) Query(ctx context.Context, q string) ([]promapi.Sample, error) {
	for pod, v := range f.byPod {
		if strings.Contains(q, pod) {
			return []promapi.Sample{{Labels: map[string]string{}, Value: v}}, nil
		}
	}
	return nil, nil
}

func TestBuild(t *testing.T) {
	start := time.Now().Add(-2 * time.Hour).Unix()
	jobs := fakeJobs{jobs: []slurmapi.Job{
		{JobID: 1, Name: "tinygpt", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-0", TresAlloc: "cpu=4,gres/gpu=1",
			StartTime: slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 2, Name: "zombie", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-1", TresAlloc: "cpu=1,gres/gpu=1",
			StartTime: slurmapi.NoVal{Set: true, Number: start}},
		{JobID: 3, Name: "cpu-only", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-2", TresAlloc: "cpu=8"},
		{JobID: 4, Name: "no-telemetry", UserName: "luis", State: []string{"RUNNING"},
			Nodes: "w-9", TresAlloc: "cpu=1,gres/gpu=1", // no fakeProm entry
			StartTime: slurmapi.NoVal{Set: true, Number: start}},
	}}
	prom := fakeProm{byPod: map[string]float64{"w-0": 90, "w-1": 0}}

	nodes := fakeNodes{"w-0": "pod-w-0", "w-1": "pod-w-1", "w-2": "pod-w-2", "w-9": "pod-w-9"}
	b := &Builder{Jobs: jobs, Prom: prom, Nodes: nodes, PodLabel: "exported_pod",
		ZombiePct: 5, ZombieWin: 15 * time.Minute}
	got, err := b.Build(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 GPU jobs, got %d", len(got))
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
}
