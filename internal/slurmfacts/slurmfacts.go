// Package slurmfacts turns what slurmrestd returns into the plain facts the
// configuration engine checks.
//
// Slurm reports in its own shapes: a job's cards arrive inside a text field
// like "gpu:1", and its processors and memory arrive packed into one string
// with unit suffixes that change between jobs on the same cluster. The engine
// wants none of that - it wants a count, a duration and a number of
// megabytes. This package is that translation, and nothing else.
//
// It lives on its own because two callers need it: squire-lint, which checks
// configuration on a login node, and squire, which now runs the same engine
// inside its monitoring pass. Two copies would drift, and the first symptom
// would be the web page and squire-lint disagreeing about one job, which is
// the kind of disagreement nobody can debug from a screenshot.
//
// It imports the Slurm client and the engine, and nothing heavier - no
// Kubernetes, no Prometheus - so squire-lint still links without either.
package slurmfacts

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/slurmapi"
)

// Job adapts a slurmrestd job into the engine's input. The adapter lives here
// rather than in internal/lint so that package keeps importing nothing but
// fmt, sort and time.
func Job(j slurmapi.Job) lint.Job {
	out := lint.Job{
		ID:            j.JobID,
		Name:          j.Name,
		User:          j.Owner(),
		Partition:     j.Partition,
		State:         j.BaseState(),
		GPUsRequested: slurmapi.GPUCount(j),
		Exclusive:     j.Exclusive(),
		ExclusiveMode: j.ExclusiveMode(),
		CPUs:          tresCount(j.TresAlloc, "cpu"),
		StateReason:   j.StateReason,
		MemoryMB:      tresMemMB(j.TresAlloc),
		SharesGPU:     sharesGPU(j.TresAlloc, j.TresPerNode),
	}
	// Slurm reports time limits in minutes, with infinite as its own flag.
	if j.TimeLimit.Set {
		out.HasTimeLimit = true
		out.Unlimited = j.TimeLimit.Infinite
		out.TimeLimit = time.Duration(j.TimeLimit.Number) * time.Minute
	}
	// A hostlist that will not expand costs this job its node-aware rules,
	// not the whole run.
	if nodes, err := slurmapi.ExpandNodes(j.Nodes); err == nil {
		out.Nodes = nodes
	}
	return out
}

// Cluster builds the engine's view of the cluster.
func Cluster(nodes []slurmapi.Node, parts []slurmapi.Partition) *lint.Cluster {
	c := &lint.Cluster{
		Nodes:      make(map[string]lint.Node, len(nodes)),
		Partitions: make(map[string]lint.Partition, len(parts)),
	}
	for _, n := range nodes {
		c.Nodes[n.Name] = lint.Node{
			Name: n.Name, GPUs: gresGPUs(n.Gres),
			MemoryMB: n.RealMemory.Number, CPUs: n.SchedulableCPUs(),
		}
	}
	for _, p := range parts {
		lp := lint.Partition{Name: p.Name}
		// An infinite maximum is not a ceiling, so the at-the-ceiling rule
		// must not fire against it.
		if p.Maximums.Time.Set && !p.Maximums.Time.Infinite {
			lp.HasMaxTime = true
			lp.MaxTime = time.Duration(p.Maximums.Time.Number) * time.Minute
		}
		c.Partitions[p.Name] = lp
	}
	return c
}

// gresGPUs pulls the GPU count out of a node's GRES string, e.g. "gpu:4",
// "gpu:tesla:4(S:0-1)" or "gpu:1(IDX:0)". Anything it cannot parse counts as
// zero, which makes the node look GPU-less rather than inventing a number.
func gresGPUs(gres string) int {
	total := 0
	for _, part := range strings.Split(gres, ",") {
		part = strings.TrimSpace(part)
		// Strip the parenthesised detail FIRST. It contains its own colons
		// ("(S:0-1)", "(IDX:0)"), so splitting on ":" before removing it
		// loses the count entirely.
		if i := strings.IndexByte(part, '('); i >= 0 {
			part = part[:i]
		}
		fields := strings.Split(part, ":")
		if len(fields) < 2 || fields[0] != "gpu" {
			continue
		}
		n := 0
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &n); err == nil {
			total += n
		}
	}
	return total
}

// tresCount pulls a plain count out of a TRES string, e.g. cpu=2 from
// "cpu=2,mem=1G,node=1". Counts carry no unit suffix, unlike memory.
func tresCount(tres, name string) int64 {
	for _, part := range strings.Split(tres, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || k != name {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// tresMemMB pulls the memory out of a TRES string such as
// "cpu=2,mem=1G,node=1,billing=2" and returns it in MB, the unit Slurm reports
// node memory in.
//
// The suffix is not optional to handle: the same cluster writes "1G" for one
// job and "191168M" for another, so reading the digits alone is wrong by a
// factor of 1024 exactly when the number is small. An unparseable value
// returns 0, which silences the rule rather than inventing a comparison.
func tresMemMB(tres string) int64 {
	for _, part := range strings.Split(tres, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name != "mem" {
			continue
		}
		// A bare number is already MB - that is Slurm's own unit for memory.
		mult, div := int64(1), int64(1)
		if len(val) > 0 {
			switch val[len(val)-1] {
			case 'K', 'k':
				div, val = 1024, val[:len(val)-1]
			case 'M', 'm':
				val = val[:len(val)-1]
			case 'G', 'g':
				mult, val = 1024, val[:len(val)-1]
			case 'T', 't':
				mult, val = 1024*1024, val[:len(val)-1]
			}
		}
		n, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			return 0
		}
		return n * mult / div
	}
	return 0
}

// sharesGPU reports whether the job holds a GPU through Slurm's shared GRES
// types rather than a whole device. Those jobs carry gres/shard or gres/mps
// and no gres/gpu at all, so every GPU count reads zero for them.
//
// Names are compared whole. A substring test would match "gres/gpu:mps_a100"
// or any future type that happens to contain these letters.
func sharesGPU(tres ...string) bool {
	for _, s := range tres {
		for _, part := range strings.Split(s, ",") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			name, _, _ = strings.Cut(name, ":")
			name = strings.TrimPrefix(name, "gres/")
			if name == "shard" || name == "mps" {
				return true
			}
		}
	}
	return false
}
