package lintcli

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmcfg"
)

// TestFlagsAreCheckOnly pins the split. Sharing the monitoring flag set would
// put thresholds, Prometheus and Kubernetes settings in this mode's help, none
// of which it reads - and a flag a mode ignores is worse than a missing one,
// because it looks like it works.
func TestFlagsAreCheckOnly(t *testing.T) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var c slurmcfg.Config
	slurmcfg.Bind(fs, &c)

	var got []string
	fs.VisitAll(func(f *flag.Flag) { got = append(got, f.Name) })
	want := map[string]bool{"slurm-url": true, "slurm-api": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("the check registers %q, which it does not read", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the check should register %d flags, got %v", len(want), got)
	}
}

func TestGresGPUs(t *testing.T) {
	cases := []struct {
		gres string
		want int
	}{
		{"gpu:4", 4},
		{"gpu:tesla:4(S:0-1)", 4},
		{"gpu:1(IDX:0)", 1},
		{"gpu:2,mps:200", 2},
		{"", 0},
		{"mps:200", 0},
		{"(null)", 0},
	}
	for _, c := range cases {
		if got := gresGPUs(c.gres); got != c.want {
			t.Errorf("gresGPUs(%q) = %d, want %d", c.gres, got, c.want)
		}
	}
}

// TestTimeLimitDecoding is the reason NoVal grew an Infinite field: without it
// an unlimited job decoded as a limit that was set, to zero - which reads as a
// job that must finish instantly.
func TestTimeLimitDecoding(t *testing.T) {
	unlimited := toLintJob(slurmapi.Job{JobID: 1,
		TimeLimit: slurmapi.NoVal{Set: true, Infinite: true}})
	if !unlimited.HasTimeLimit || !unlimited.Unlimited {
		t.Errorf("infinite limit must arrive as set AND unlimited: %+v", unlimited)
	}
	twoHours := toLintJob(slurmapi.Job{JobID: 2,
		TimeLimit: slurmapi.NoVal{Set: true, Number: 120}})
	if twoHours.TimeLimit != 2*time.Hour || twoHours.Unlimited {
		t.Errorf("120 minutes must arrive as 2h, got %s", twoHours.TimeLimit)
	}
	none := toLintJob(slurmapi.Job{JobID: 3})
	if none.HasTimeLimit {
		t.Errorf("an unset limit must not read as set: %+v", none)
	}
}

// TestStateReachesTheEngine: the state decides whether a job is judged at all,
// and Slurm appends flags after it. Only the first element is the state.
func TestStateReachesTheEngine(t *testing.T) {
	j := toLintJob(slurmapi.Job{JobID: 1, State: []string{"RUNNING", "CONFIGURING"}})
	if j.State != "RUNNING" {
		t.Errorf("the base state is the first element, got %q", j.State)
	}
	if got := toLintJob(slurmapi.Job{JobID: 2}).State; got != "" {
		t.Errorf("no state must stay empty rather than guess, got %q", got)
	}
}

// TestExclusiveFromShared pins the field that is not called what you expect:
// there is no "exclusive" on the job-info schema, and --exclusive renders as
// shared="none".
func TestExclusiveFromShared(t *testing.T) {
	if !(slurmapi.Job{Shared: []string{"none"}}).Exclusive() {
		t.Errorf(`shared "none" means exclusive`)
	}
	if (slurmapi.Job{Shared: []string{"user"}}).Exclusive() {
		t.Errorf(`shared "user" is not exclusive`)
	}
	if (slurmapi.Job{}).Exclusive() {
		t.Errorf("no shared field is not exclusive")
	}
}

// TestTresMemMB pins the unit handling. The same cluster writes "1G" for one
// job and "191168M" for another, so reading the digits alone is wrong by 1024x
// exactly when the number is small - and a wrong number here means the rule
// silently never fires.
func TestTresMemMB(t *testing.T) {
	cases := []struct {
		tres string
		want int64
	}{
		{"cpu=2,mem=1G,node=1,billing=2", 1024},
		{"cpu=4,mem=191168M,node=1,billing=4", 191168},
		{"cpu=1,mem=15617,node=1", 15617}, // bare is already MB
		{"cpu=1,mem=1T,node=1", 1024 * 1024},
		{"cpu=1,mem=2048K,node=1", 2},
		{"cpu=1,node=1,billing=1", 0}, // no memory named
		{"", 0},
		{"cpu=1,mem=,node=1", 0},
		{"cpu=1,mem=lots,node=1", 0},
	}
	for _, c := range cases {
		if got := tresMemMB(c.tres); got != c.want {
			t.Errorf("tresMemMB(%q) = %d, want %d", c.tres, got, c.want)
		}
	}
}

// TestSharesGPU covers the types that use a GPU without holding one. A job
// with shards reports no GPUs of its own, so without this it looks like a
// CPU-only job squatting on a GPU node - the wrong accusation entirely.
func TestSharesGPU(t *testing.T) {
	cases := []struct {
		tres string
		want bool
	}{
		{"cpu=2,mem=1G,node=1,billing=2,gres/shard=1", true},
		{"cpu=2,mem=1G,gres/mps=50", true},
		{"gres/shard:tesla=2", true},
		{"cpu=2,mem=1G,node=1,gres/gpu=1", false},
		{"cpu=2,mem=1G,node=1", false},
		{"", false},
		// Whole names only: a substring test would call this one shared.
		{"cpu=1,gres/gpu:mps_a100=1", false},
	}
	for _, c := range cases {
		if got := sharesGPU(c.tres); got != c.want {
			t.Errorf("sharesGPU(%q) = %v, want %v", c.tres, got, c.want)
		}
	}
	if !sharesGPU("cpu=1", "gres/shard:1") {
		t.Errorf("any of the TRES strings naming a shared type counts")
	}
}

func TestToClusterReadsLimitsAndMemory(t *testing.T) {
	nodes := []slurmapi.Node{
		{Name: "a", Gres: "gpu:1", Partitions: []string{"batch"},
			RealMemory: slurmapi.NoVal{Set: true, Number: 15617}},
		{Name: "b", Gres: "gpu:4", Partitions: []string{"batch"}},
		{Name: "c", Gres: "", Partitions: []string{"cpu"}},
	}
	parts := []slurmapi.Partition{{Name: "batch"}, {Name: "cpu"}}
	parts[0].Maximums.Time = slurmapi.NoVal{Set: true, Number: 1440}
	parts[1].Maximums.Time = slurmapi.NoVal{Set: true, Infinite: true}

	c := toCluster(nodes, parts)
	if !c.Partitions["batch"].HasMaxTime || c.Partitions["batch"].MaxTime != 24*time.Hour {
		t.Errorf("1440 minutes must be 24h: %+v", c.Partitions["batch"])
	}
	// An infinite maximum is not a ceiling to compare against.
	if c.Partitions["cpu"].HasMaxTime {
		t.Errorf("an infinite partition maximum must not read as a ceiling")
	}
	if c.Nodes["c"].GPUs != 0 {
		t.Errorf("a node with no gres has no GPUs")
	}
	if c.Nodes["a"].MemoryMB != 15617 {
		t.Errorf("node memory must survive the adapter, got %d", c.Nodes["a"].MemoryMB)
	}
	if c.Nodes["b"].MemoryMB != 0 {
		t.Errorf("a node that reported no memory must stay 0, got %d", c.Nodes["b"].MemoryMB)
	}
}

// fakeSlurm serves canned slurmrestd responses. jobs is the body for /jobs;
// an empty body for either collection makes that endpoint fail, which is how
// the degraded and error paths are exercised.
func fakeSlurm(t *testing.T, jobs, nodes, parts string) *slurmapi.Client {
	t.Helper()
	body := map[string]string{
		"/slurm/v0.0.44/jobs":       jobs,
		"/slurm/v0.0.44/nodes":      nodes,
		"/slurm/v0.0.44/partitions": parts,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := body[r.URL.Path]
		if !ok || b == "" {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(b))
	}))
	t.Cleanup(srv.Close)
	return slurmapi.NewClient(srv.URL, "v0.0.44", "token")
}

const (
	oneCleanJob = `{"jobs":[{"job_id":1,"name":"ok","partition":"batch",
		"job_state":["RUNNING"],"nodes":"a","tres_per_node":"gres/gpu:1",
		"time_limit":{"set":true,"number":60}}]}`
	oneBadJob = `{"jobs":[{"job_id":2,"name":"nolimit","partition":"batch",
		"job_state":["RUNNING"],"nodes":"a","tres_per_node":"gres/gpu:1"}]}`
	oneFinishedBadJob = `{"jobs":[{"job_id":3,"name":"done","partition":"batch",
		"job_state":["COMPLETED"],"nodes":"a"}]}`
	someNodes = `{"nodes":[{"name":"a","gres":"gpu:1","partitions":["batch"]}]}`
	someParts = `{"partitions":[{"name":"batch","maximums":{"time":{"set":true,"number":1440}}}]}`
)

// TestExitCodes is the contract anything scripting this depends on, and the
// one part of the output a script cannot see.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		jobs       string
		nodes      string
		parts      string
		want       int
		wantOutput string
	}{
		{name: "clean queue", jobs: oneCleanJob, nodes: someNodes, parts: someParts,
			want: exitClean, wantOutput: "no findings"},
		{name: "findings", jobs: oneBadJob, nodes: someNodes, parts: someParts,
			want: exitFindings, wantOutput: "no-time-limit"},
		{name: "slurmrestd unreachable", jobs: "", nodes: someNodes, parts: someParts,
			want: exitError, wantOutput: "error:"},
		{name: "cluster data missing still runs spec rules", jobs: oneBadJob, nodes: "", parts: "",
			want: exitFindings, wantOutput: "job-spec rules only"},
		// A finished job is still in Slurm's response for MinJobAge. Judging it
		// would accuse someone over an allocation they already gave back.
		{name: "finished job is not judged", jobs: oneFinishedBadJob, nodes: someNodes, parts: someParts,
			want: exitClean, wantOutput: "0 of 1 jobs"},
	}
	for _, tc := range cases {
		var sb strings.Builder
		got := runLint(context.Background(), fakeSlurm(t, tc.jobs, tc.nodes, tc.parts), &sb)
		if got != tc.want {
			t.Errorf("%s: exit %d, want %d (output: %s)", tc.name, got, tc.want, sb.String())
		}
		if !strings.Contains(sb.String(), tc.wantOutput) {
			t.Errorf("%s: output %q does not contain %q", tc.name, sb.String(), tc.wantOutput)
		}
	}
}
