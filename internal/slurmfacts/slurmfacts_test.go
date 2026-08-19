package slurmfacts

import (
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/slurmapi"
)

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
	unlimited := Job(slurmapi.Job{JobID: 1,
		TimeLimit: slurmapi.NoVal{Set: true, Infinite: true}})
	if !unlimited.HasTimeLimit || !unlimited.Unlimited {
		t.Errorf("infinite limit must arrive as set AND unlimited: %+v", unlimited)
	}
	twoHours := Job(slurmapi.Job{JobID: 2,
		TimeLimit: slurmapi.NoVal{Set: true, Number: 120}})
	if twoHours.TimeLimit != 2*time.Hour || twoHours.Unlimited {
		t.Errorf("120 minutes must arrive as 2h, got %s", twoHours.TimeLimit)
	}
	none := Job(slurmapi.Job{JobID: 3})
	if none.HasTimeLimit {
		t.Errorf("an unset limit must not read as set: %+v", none)
	}
}

// TestStateReachesTheEngine: the state decides whether a job is judged at all,
// and Slurm appends flags after it. Only the first element is the state.
func TestStateReachesTheEngine(t *testing.T) {
	j := Job(slurmapi.Job{JobID: 1, State: []string{"RUNNING", "CONFIGURING"}})
	if j.State != "RUNNING" {
		t.Errorf("the base state is the first element, got %q", j.State)
	}
	if got := Job(slurmapi.Job{JobID: 2}).State; got != "" {
		t.Errorf("no state must stay empty rather than guess, got %q", got)
	}
}

// TestExclusiveFromShared pins the field that is not called what you expect:
// there is no "exclusive" on the job-info schema, and the three forms of
// exclusivity arrive as shared="none", "user" and "mcs".
//
// All three strand the GPUs the job did not request. They differ only in who
// is excluded, which is why the mode travels with the finding.
func TestExclusiveFromShared(t *testing.T) {
	cases := []struct {
		shared []string
		mode   string
	}{
		{[]string{"none"}, "none"},
		{[]string{"user"}, "user"},
		{[]string{"mcs"}, "mcs"},
		{[]string{"oversubscribe"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		j := slurmapi.Job{Shared: c.shared}
		if got := j.ExclusiveMode(); got != c.mode {
			t.Errorf("shared %v: mode %q, want %q", c.shared, got, c.mode)
		}
		if got := j.Exclusive(); got != (c.mode != "") {
			t.Errorf("shared %v: Exclusive() = %v", c.shared, got)
		}
	}
}

// TestTresCount covers the plain-count parser. Counts carry no unit suffix,
// which is the whole reason it is not tresMemMB.
func TestTresCount(t *testing.T) {
	cases := []struct {
		tres, name string
		want       int64
	}{
		{"cpu=2,mem=1G,node=1,billing=2", "cpu", 2},
		{"cpu=48,mem=191168M,node=1", "cpu", 48},
		{"cpu=2,mem=1G", "node", 0},
		{"", "cpu", 0},
		{"cpu=,mem=1G", "cpu", 0},
		{"cpu=many", "cpu", 0},
	}
	for _, c := range cases {
		if got := tresCount(c.tres, c.name); got != c.want {
			t.Errorf("tresCount(%q, %q) = %d, want %d", c.tres, c.name, got, c.want)
		}
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

	c := Cluster(nodes, parts)
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
