package slurmapi

import (
	"reflect"
	"strings"
	"testing"
)

func TestGPUCount(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want int
	}{
		// Real shapes captured from slurmrestd v0.0.44 on a live cluster.
		{"v0.0.44 running gpu job", Job{
			TresAlloc:   "cpu=4,mem=15785M,node=1,billing=4", // no gres here
			TresPerNode: "gres/gpu:1",
			GresDetail:  []string{"gpu:1(IDX:0)"}}, 1},
		{"two nodes one gpu each", Job{
			GresDetail: []string{"gpu:1(IDX:0)", "gpu:1(IDX:0)"}}, 2},
		{"four gpus one node", Job{
			GresDetail: []string{"gpu:4(IDX:0-3)"}}, 4},
		// Older/other shapes that must keep working.
		{"tres_alloc plain", Job{TresAlloc: "cpu=4,mem=8G,gres/gpu=1"}, 1},
		{"typed gres", Job{TresAlloc: "cpu=8,gres/gpu:tesla=2"}, 2},
		{"per-node only, node count unknown", Job{TresPerNode: "gres/gpu:2"}, 2},
		{"no gpu", Job{TresAlloc: "cpu=4,mem=8G"}, 0},
		{"empty", Job{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GPUCount(tt.job); got != tt.want {
				t.Errorf("GPUCount(%+v) = %d, want %d", tt.job, got, tt.want)
			}
		})
	}
}

func TestOwner(t *testing.T) {
	tests := []struct {
		name string
		job  Job
		want string
	}{
		{"username present", Job{UserName: "root"}, "root"},
		{"username empty, uid set", Job{UserID: NoVal{Set: true, Number: 50000}}, "uid:50000"},
		{"neither", Job{}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.Owner(); got != tt.want {
				t.Errorf("Owner() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpandNodes(t *testing.T) {
	tests := []struct {
		name  string
		nodes string
		want  []string
	}{
		{"range", "gpu-[0-1]", []string{"gpu-0", "gpu-1"}},
		{"single", "gpu-0", []string{"gpu-0"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandNodes(tt.nodes)
			if err != nil {
				t.Fatalf("ExpandNodes(%q) error: %v", tt.nodes, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ExpandNodes(%q) = %v, want %v", tt.nodes, got, tt.want)
			}
		})
	}
}

// test for gpu indices parsing!
func TestGPUIndices(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want map[string][]string
	}{
		{
			name: "single node, single gpu",
			job:  Job{Nodes: "gpu-0", GresDetail: []string{"gpu:1(IDX:0)"}},
			want: map[string][]string{"gpu-0": {"0"}},
		},
		{
			name: "range of devices",
			job:  Job{Nodes: "gpu-0", GresDetail: []string{"gpu:2(IDX:0-1)"}},
			want: map[string][]string{"gpu-0": {"0", "1"}},
		},
		{
			name: "mixed singles and ranges",
			job:  Job{Nodes: "gpu-0", GresDetail: []string{"gpu:3(IDX:0,2-3)"}},
			want: map[string][]string{"gpu-0": {"0", "2", "3"}},
		},
		{
			name: "typed gres still parses",
			job:  Job{Nodes: "gpu-0", GresDetail: []string{"gpu:tesla:2(IDX:2-3)"}},
			want: map[string][]string{"gpu-0": {"2", "3"}},
		},
		{
			// The case that matters: two nodes, DIFFERENT devices on each.
			name: "multi-node with differing index sets",
			job: Job{Nodes: "gpu-[0-1]",
				GresDetail: []string{"gpu:2(IDX:0-1)", "gpu:1(IDX:3)"}},
			want: map[string][]string{"gpu-0": {"0", "1"}, "gpu-1": {"3"}},
		},
		{
			name: "no gres_detail falls back to nil, never to empty",
			job:  Job{Nodes: "gpu-0", TresAlloc: "cpu=4,gres/gpu=1"},
			want: nil,
		},
		{
			name: "gres_detail without IDX falls back",
			job:  Job{Nodes: "gpu-0", GresDetail: []string{"gpu:1"}},
			want: nil,
		},
		{
			// A disagreement between the two fields must never be guessed at:
			// mis-aligning them attributes one node's GPUs to another node.
			name: "node/detail length mismatch falls back",
			job:  Job{Nodes: "gpu-[0-1]", GresDetail: []string{"gpu:1(IDX:0)"}},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GPUIndices(tc.job)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for node, idx := range tc.want {
				if strings.Join(got[node], ",") != strings.Join(idx, ",") {
					t.Errorf("node %s: got %v, want %v", node, got[node], idx)
				}
			}
		})
	}
}
