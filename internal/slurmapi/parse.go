package slurmapi

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/puttsk/hostlist"
)

// gpuNum matches every spelling Slurm uses for a GPU count: "gpu=1" (equals,
// as in tres_alloc_str), "gpu:1" (colon, as in tres_per_node and gres_detail),
// and typed "gpu:tesla=2". The optional middle group swallows a device type
// without consuming the digits.
var gpuNum = regexp.MustCompile(`gpu(?::[^:=,()]+)?[:=]([0-9]+)`)

// gpusIn sums every GPU count appearing in s.
func gpusIn(s string) int {
	total := 0
	for _, m := range gpuNum.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			total += n
		}
	}
	return total
}

// GPUCount returns how many GPUs a job holds.
//
// Three fields are consulted on purpose. Slurm reports GPU allocation in three
// places with three spellings, and which of them are populated varies by API
// version. On slurmrestd v0.0.44, tres_alloc_str carries NO gres entry at all —
// a running GPU job reports only "cpu=4,mem=15785M,node=1,billing=4" — so a
// parser trusting that field alone finds zero GPUs for every job, the report
// builder filters them all out, and warden prints an empty table with no error
// at all. Verified against a live cluster; do not "simplify" this to one field.
//
//	gres_detail    ["gpu:1(IDX:0)"]        one entry per allocated node
//	tres_per_node  "gres/gpu:1"            per node, colon-separated
//	tres_alloc_str "cpu=4,gres/gpu=1"      totals, equals-separated
//
// gres_detail is preferred because summing its entries yields the true total
// without needing a separate node count. tres_per_node is per node, so it is
// multiplied by the node count when that is the only field available.
func GPUCount(j Job) int {
	if n := gpusIn(strings.Join(j.GresDetail, ",")); n > 0 {
		return n
	}
	if n := gpusIn(j.TresPerNode); n > 0 {
		nodes := len(j.GresDetail)
		if nodes == 0 {
			nodes = 1
		}
		return n * nodes
	}
	return gpusIn(j.TresAlloc)
}

// ExpandNodes turns a Slurm hostlist ("gpu-[0-1]") into individual Slurm node
// names ([gpu-0 gpu-1]).
//
// These are SLURM node names, not Kubernetes pod names — the two differ. On a
// Slinky cluster Slurm knows a node as "gpu-0" while its pod is
// "slurm-worker-gpu-0"; the prefix depends on the Helm release name and the
// NodeSet key, so it cannot be reconstructed by string manipulation. Translate
// with kube.NodeMap, which reads the operator's own pod-hostname label.
func ExpandNodes(nodes string) ([]string, error) {
	if nodes == "" {
		return nil, nil
	}
	return hostlist.Expand(nodes)
}

// Owner returns the job's owner for display.
//
// slurmrestd resolves UIDs against the passwd database of ITS OWN container,
// which is the stock slurmrestd image — it does not contain cluster users who
// exist only in the worker and login images. So user_name comes back empty for
// exactly the users warden most needs to name (root resolves everywhere and
// looks fine, which hides the problem). The numeric UID is always present and
// always correct, so fall back to it rather than printing a blank cell.
func (j Job) Owner() string {
	if j.UserName != "" {
		return j.UserName
	}
	if j.UserID.Set {
		return fmt.Sprintf("uid:%d", j.UserID.Number)
	}
	return "-"
}
