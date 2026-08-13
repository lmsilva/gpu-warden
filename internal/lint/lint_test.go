package lint

import (
	"strings"
	"testing"
	"time"
)

// testCluster deliberately uses node names that carry no hint of what they
// are. compute-042 has four GPUs, node1 has none. Any rule that pattern-matches
// a name instead of reading the node's own GRES fails here.
func testCluster() *Cluster {
	return &Cluster{
		Nodes: map[string]Node{
			"compute-042": {Name: "compute-042", GPUs: 4, MemoryMB: 191168},
			"node1":       {Name: "node1", GPUs: 0, MemoryMB: 15617},
		},
		Partitions: map[string]Partition{
			"batch":   {Name: "batch", MaxTime: 24 * time.Hour, HasMaxTime: true},
			"nolimit": {Name: "nolimit"}, // no ceiling, so the at-max rule cannot fire
		},
	}
}

func rules(fs []Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func has(fs []Finding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func TestRules(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want []string // rules expected, in sorted order
	}{
		{
			name: "clean job fires nothing",
			job: Job{ID: 1, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 4, TimeLimit: 2 * time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "no time limit",
			job:  Job{ID: 2, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"}, GPUsRequested: 1},
			want: []string{"no-time-limit"},
		},
		{
			name: "unlimited time limit is the same finding",
			job: Job{ID: 3, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 1, HasTimeLimit: true, Unlimited: true},
			want: []string{"no-time-limit"},
		},
		{
			name: "time limit exactly at the partition ceiling",
			job: Job{ID: 4, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 1, TimeLimit: 24 * time.Hour, HasTimeLimit: true},
			want: []string{"time-limit-at-partition-max"},
		},
		{
			name: "cpu-only job on a node that has GPUs",
			job: Job{ID: 5, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 0, TimeLimit: time.Hour, HasTimeLimit: true},
			want: []string{"cpu-only-on-gpu-node"},
		},
		{
			name: "a job holding shards is using the GPU, not blocking it",
			job: Job{ID: 14, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 0, SharesGPU: true, TimeLimit: time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "cpu-only job on a node with no GPUs is fine",
			job: Job{ID: 6, Partition: "batch", State: "RUNNING", Nodes: []string{"node1"},
				GPUsRequested: 0, TimeLimit: time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "exclusive but asked for fewer GPUs than the node has",
			job: Job{ID: 7, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 1, Exclusive: true, TimeLimit: time.Hour, HasTimeLimit: true},
			want: []string{"exclusive-over-allocation"},
		},
		{
			name: "exclusive taking the whole node is fine",
			job: Job{ID: 8, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 4, Exclusive: true, TimeLimit: time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "holds every MB on a node whose GPUs it only partly requested",
			job: Job{ID: 11, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 1, MemoryMB: 191168, TimeLimit: time.Hour, HasTimeLimit: true},
			want: []string{"memory-over-allocation"},
		},
		{
			name: "holding every MB is fine when the job took every GPU too",
			job: Job{ID: 12, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 4, MemoryMB: 191168, TimeLimit: time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "a modest slice of memory strands nothing",
			job: Job{ID: 13, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 1, MemoryMB: 16384, TimeLimit: time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "a partition with no ceiling cannot be sat at",
			job: Job{ID: 9, Partition: "nolimit", State: "RUNNING", Nodes: []string{"compute-042"},
				GPUsRequested: 4, TimeLimit: 24 * time.Hour, HasTimeLimit: true},
			want: nil,
		},
		{
			name: "dependency Slurm says can never be satisfied",
			job: Job{ID: 10, Partition: "batch", State: "PENDING", GPUsRequested: 1,
				TimeLimit: time.Hour, HasTimeLimit: true,
				StateReason: "DependencyNeverSatisfied"},
			want: []string{"dependency-doomed"},
		},
	}

	c := testCluster()
	for _, tc := range cases {
		got := Check(tc.job, c)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, rules(got), tc.want)
			continue
		}
		for i, r := range tc.want {
			if got[i].Rule != r {
				t.Errorf("%s: finding %d is %q, want %q", tc.name, i, got[i].Rule, r)
			}
		}
		for _, f := range got {
			t.Logf("%-28s %-4s job %d: %s", f.Rule, f.Severity, f.JobID, f.Message)
		}
	}
}

// TestFinishedJobsAreNotChecked is the reason the engine reads State at all.
// Slurm keeps finished jobs in its response for MinJobAge, and every one of
// them would otherwise be judged: a completed CPU job would be told it is
// blocking a GPU node it gave back minutes ago.
//
// The states listed are checked; anything else, including a state that does
// not exist yet, is silence.
func TestFinishedJobsAreNotChecked(t *testing.T) {
	c := testCluster()
	// A fixture that fires three rules while it is running.
	base := Job{ID: 1, Partition: "batch", Nodes: []string{"compute-042"},
		GPUsRequested: 0, StateReason: "DependencyNeverSatisfied"}

	for _, state := range []string{"PENDING", "RUNNING", "SUSPENDED", "CONFIGURING"} {
		j := base
		j.State = state
		if got := Check(j, c); len(got) == 0 {
			t.Errorf("%s is live work and must be checked", state)
		}
	}
	for _, state := range []string{"COMPLETED", "CANCELLED", "FAILED", "TIMEOUT",
		"NODE_FAIL", "OUT_OF_MEMORY", "COMPLETING", "", "SOME_FUTURE_STATE"} {
		j := base
		j.State = state
		if got := Check(j, c); len(got) != 0 {
			t.Errorf("%q is not live work and must produce nothing, got %v", state, rules(got))
		}
	}
}

// TestNoClusterRunsSpecRulesOnly is the degradation path: with no cluster data
// the spec-shaped rules still work and the cluster-aware ones stay silent
// rather than guessing.
func TestNoClusterRunsSpecRulesOnly(t *testing.T) {
	j := Job{ID: 1, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
		GPUsRequested: 0, Exclusive: true}
	got := Check(j, nil)
	if !has(got, "no-time-limit") {
		t.Errorf("the spec-only rule must still fire: %v", rules(got))
	}
	for _, f := range got {
		if f.Rule != "no-time-limit" {
			t.Errorf("%s needs cluster data and must not fire without it", f.Rule)
		}
	}
}

// TestNamesAreNeverPatternMatched guards the principle directly: a node called
// node1 has no GPUs and a node called compute-042 has four. A rule keying on
// the name rather than the GRES gets both backwards.
func TestNamesAreNeverPatternMatched(t *testing.T) {
	c := testCluster()
	onGPUNode := Check(Job{ID: 1, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
		HasTimeLimit: true, TimeLimit: time.Hour}, c)
	if !has(onGPUNode, "cpu-only-on-gpu-node") {
		t.Errorf("compute-042 has 4 GPUs and must be treated as a GPU node")
	}
	onPlainNode := Check(Job{ID: 2, Partition: "batch", State: "RUNNING", Nodes: []string{"node1"},
		HasTimeLimit: true, TimeLimit: time.Hour}, c)
	if has(onPlainNode, "cpu-only-on-gpu-node") {
		t.Errorf("node1 has no GPUs and must not be treated as a GPU node")
	}
}

// TestUnknownMemoryIsSilent: memory Squire could not read is 0, and 0 must not
// read as "asked for nothing" and quietly satisfy a comparison.
func TestUnknownMemoryIsSilent(t *testing.T) {
	c := testCluster()
	noJobMem := Check(Job{ID: 1, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
		GPUsRequested: 1, TimeLimit: time.Hour, HasTimeLimit: true}, c)
	if has(noJobMem, "memory-over-allocation") {
		t.Errorf("unknown job memory must produce nothing: %v", rules(noJobMem))
	}

	unknownNode := &Cluster{
		Nodes:      map[string]Node{"compute-042": {Name: "compute-042", GPUs: 4}},
		Partitions: c.Partitions,
	}
	noNodeMem := Check(Job{ID: 2, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
		GPUsRequested: 1, MemoryMB: 191168, TimeLimit: time.Hour, HasTimeLimit: true}, unknownNode)
	if has(noNodeMem, "memory-over-allocation") {
		t.Errorf("unknown node memory must produce nothing: %v", rules(noNodeMem))
	}
}

// TestUnknownPartitionIsSilent: a partition Squire has no data for produces no
// cluster-aware findings, rather than a finding computed against zero.
func TestUnknownPartitionIsSilent(t *testing.T) {
	c := testCluster()
	got := Check(Job{ID: 1, Partition: "nonexistent", State: "RUNNING", GPUsRequested: 1,
		HasTimeLimit: true, TimeLimit: 24 * time.Hour}, c)
	if len(got) != 0 {
		t.Errorf("unknown partition must produce nothing, got %v", rules(got))
	}
}

func TestFindingsAreStablyOrdered(t *testing.T) {
	c := testCluster()
	j := Job{ID: 1, Partition: "batch", State: "RUNNING", Nodes: []string{"compute-042"},
		GPUsRequested: 0, StateReason: "DependencyNeverSatisfied"}
	first := rules(Check(j, c))
	for i := 0; i < 5; i++ {
		if got := rules(Check(j, c)); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("ordering is not stable: %v then %v", first, got)
		}
	}
	if len(first) < 2 {
		t.Fatalf("this fixture should produce several findings, got %v", first)
	}
}
