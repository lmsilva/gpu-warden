// Package lint reports configuration problems in Slurm jobs: what a job asked
// for, checked against its own spec and against what the cluster actually is.
// It answers a different question from internal/verdict, which reports what a
// GPU did — findings here need no telemetry at all.
//
// Pure, like verdict: no network, no Slurm client, no Kubernetes. It takes
// plain structs and returns findings, so every rule is testable on a laptop
// and the same engine serves the CLI, a web view, or anything else.
package lint

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Severity grades a finding. Two levels, not five: more would invite arguing
// about the grade instead of the finding.
type Severity int

const (
	// Note is worth knowing. Warn is worth fixing.
	Note Severity = iota
	Warn
)

func (s Severity) String() string {
	if s == Warn {
		return "warn"
	}
	return "note"
}

// checkable lists the job states worth checking: work the cluster has not
// finished with, where a finding can still change something. Everything else
// is history - a job that has already exited cannot be given a time limit, and
// saying it blocks a GPU node is simply false.
//
// It is an allowlist of states to check rather than a list of states to skip,
// so a state added to Slurm later produces silence instead of an accusation
// against a job nobody can act on.
var checkable = map[string]bool{
	"PENDING":     true,
	"RUNNING":     true,
	"SUSPENDED":   true,
	"CONFIGURING": true,
}

// Checkable reports whether a job's state is one this package judges. Exported
// so a caller counts the same jobs the engine looked at.
func Checkable(state string) bool { return checkable[state] }

// Finding is one problem with one job. Rule is a stable identifier callers can
// filter on; Message is the sentence a human reads.
type Finding struct {
	JobID    int
	Rule     string
	Severity Severity
	Message  string
}

// Job is what a job asked for. Deliberately not slurmapi.Job: importing that
// would drag net/http and the hostlist dependency into a package whose whole
// point is having none.
type Job struct {
	ID        int
	Name      string
	User      string
	Partition string

	// State is the job's base state, without Slurm's trailing flags. It
	// decides whether the job is checked at all - see Checkable.
	State string

	// Nodes the job holds or would hold, already expanded by the caller.
	Nodes []string

	GPUsRequested int  // 0 for a CPU-only job
	Exclusive     bool // whole-node allocation requested

	// GPUsPerNode is what the job asked for on each node it lands on, read
	// from the per-node request alone. GPUsRequested can come from what was
	// allocated instead, which is a job-wide number - only this one can be
	// compared against a single node's card count. 0 means none was named.
	GPUsPerNode int

	// ExclusiveMode is Slurm's word for how the job shares its nodes:
	// "none" for --exclusive, "user" and "mcs" for the scoped variants.
	// Empty means the node is shared normally.
	ExclusiveMode string

	// SharesGPU means the job holds a GPU through a shared resource rather
	// than a whole device - Slurm's shard and mps types. Such a job reports
	// no GPUs of its own, so without this it looks CPU-only on a GPU node.
	SharesGPU bool

	// CPUs is the CPU count the job was ALLOCATED. 0 means unknown.
	CPUs int64

	// MemoryMB is the memory the job was ALLOCATED, not what it asked for.
	// Slurm hands a job that named no memory the node's entire supply, so the
	// two differ in exactly the case worth reporting. 0 means unknown.
	MemoryMB int64

	// TimeLimit is the wall-clock limit. HasTimeLimit false means none was
	// set; Unlimited means it was set to infinite. The two are different
	// mistakes and the rules treat them differently.
	TimeLimit    time.Duration
	HasTimeLimit bool
	Unlimited    bool

	// StateReason is Slurm's own word for why a pending job is pending.
	StateReason string
}

// Node is what a node actually is. GPUs comes from the node's own GRES rather
// than its name - though note that is a resource type named "gpu", not proof
// of a GPU. Slurm reserves that name in practice: autodetection, device
// binding and --gpus all key on it. A site free to call it something else
// would read as zero here, which silences the GPU rules rather than
// misreporting them.
type Node struct {
	Name string
	GPUs int
	// Partitions the node belongs to. A request can only be satisfied by a
	// node in the partition it was submitted to, so a rule comparing the two
	// needs to know which nodes count.
	Partitions []string
	// MemoryMB is the node's total memory. 0 means unknown, which silences
	// the rule that compares against it.
	MemoryMB int64
	// CPUs is what the scheduler can hand out from this node. 0 means
	// unknown, same contract as MemoryMB.
	CPUs int64
}

// Partition is what a partition allows.
type Partition struct {
	Name string
	// MaxTime is the partition's wall-clock ceiling. HasMaxTime false means
	// unlimited, in which case the at-the-ceiling rule cannot fire.
	MaxTime    time.Duration
	HasMaxTime bool
}

// Cluster is everything the cluster-aware rules need, keyed for lookup.
type Cluster struct {
	Nodes      map[string]Node
	Partitions map[string]Partition
}

// Check runs every rule over one job and returns its findings, ordered by rule
// name so output is stable between runs.
//
// Two kinds of silence, both deliberate. A job in a state this package does
// not check produces nothing at all. A nil Cluster runs only the rules that
// need nothing but the job spec, leaving the rest quiet rather than judging
// against zero.
func Check(j Job, c *Cluster) []Finding {
	if !Checkable(j.State) {
		return nil
	}

	var out []Finding
	add := func(f Finding) { out = append(out, f) }

	dependencyDoomed(j, add)
	noTimeLimit(j, add)
	if c != nil {
		unsatisfiableRequest(j, *c, add)
		timeLimitAtPartitionMax(j, *c, add)
		cpuOnlyOnGPUNode(j, *c, add)
		exclusiveOverAllocation(j, *c, add)
		memoryOverAllocation(j, *c, add)
		cpuOverAllocation(j, *c, add)
	}

	sort.SliceStable(out, func(a, b int) bool { return out[a].Rule < out[b].Rule })
	return out
}

// CheckAll runs Check over many jobs, preserving the caller's job order and
// sorting each job's findings by rule.
func CheckAll(jobs []Job, c *Cluster) []Finding {
	var out []Finding
	for _, j := range jobs {
		out = append(out, Check(j, c)...)
	}
	return out
}

// dependencyDoomed: Slurm has already decided this job can never run. Squire
// detects it and points at the setting that would clean it up automatically —
// it does not reimplement kill_invalid_depend.
func dependencyDoomed(j Job, add func(Finding)) {
	if j.StateReason != "DependencyNeverSatisfied" {
		return
	}
	add(Finding{JobID: j.ID, Rule: "dependency-doomed", Severity: Warn,
		Message: "pending on a dependency Slurm says can never be satisfied - it will queue forever. " +
			"kill_invalid_depend in slurm.conf removes these automatically"})
}

// unsatisfiableRequest: the job asks each node for more cards than any node in
// its partition has, so no amount of waiting will start it. Slurm accepts the
// submission and queues it, and the pending reason names resources, which is
// the same word a job waiting behind a busy queue gets - so the two are
// indistinguishable to the person watching.
//
// Only the per-node card count is checked. It is the one request that maps
// exactly onto one node: Slurm reports processors and memory as job-wide
// totals, and a job spread over four nodes may legitimately ask for more of
// both than any single node has. Reporting on a number this rule cannot
// interpret would produce confident nonsense on multi-node jobs.
//
// A partition with no nodes in the list is silence, not a finding: it means
// the node list was filtered or unreadable, not that the partition is empty.
func unsatisfiableRequest(j Job, c Cluster, add func(Finding)) {
	if j.GPUsPerNode <= 0 {
		return
	}
	best, seen := 0, 0
	for _, n := range c.Nodes {
		if !slices.Contains(n.Partitions, j.Partition) {
			continue
		}
		seen++
		if n.GPUs > best {
			best = n.GPUs
		}
	}
	if seen == 0 || j.GPUsPerNode <= best {
		return
	}
	add(Finding{JobID: j.ID, Rule: "unsatisfiable-request", Severity: Warn,
		Message: fmt.Sprintf("asks for %d GPUs per node, and the largest node in %s has %d - this job cannot start, whatever the queue does",
			j.GPUsPerNode, j.Partition, best)})
}

// noTimeLimit: a job with no wall-clock limit cannot be backfilled, because
// the scheduler has no idea when it ends. That costs the whole cluster
// throughput, not just this job.
func noTimeLimit(j Job, add func(Finding)) {
	switch {
	case j.Unlimited:
		add(Finding{JobID: j.ID, Rule: "no-time-limit", Severity: Warn,
			Message: "unlimited time limit - the scheduler cannot backfill around a job with no end"})
	case !j.HasTimeLimit:
		add(Finding{JobID: j.ID, Rule: "no-time-limit", Severity: Warn,
			Message: "no time limit set - the scheduler cannot backfill around a job with no end"})
	}
}

// timeLimitAtPartitionMax: a limit exactly at the partition ceiling is usually
// the default rather than an estimate, and it is the worst possible number for
// backfill. Stated as a note: it might genuinely be a very long job.
func timeLimitAtPartitionMax(j Job, c Cluster, add func(Finding)) {
	if !j.HasTimeLimit || j.Unlimited {
		return // no-time-limit already covered it
	}
	p, ok := c.Partitions[j.Partition]
	if !ok || !p.HasMaxTime || j.TimeLimit != p.MaxTime {
		return
	}
	add(Finding{JobID: j.ID, Rule: "time-limit-at-partition-max", Severity: Note,
		Message: fmt.Sprintf("time limit is exactly the partition maximum (%s), which is usually the default rather than an estimate - a tighter limit backfills sooner",
			p.MaxTime)})
}

// cpuOnlyOnGPUNode: the job asked for no GPUs, yet landed on a node that has
// them. It is spending cores and memory on scarce hardware that only GPU work
// can make use of. How much that costs depends on what it left behind: take a
// small slice and the GPUs stay schedulable for someone else, take most of the
// node and they are stranded until it exits.
//
// "Has GPUs" is the node's own GRES, never its name - a site can call a GPU
// node anything. Jobs holding shards or MPS returned above: they are using a
// GPU without being counted as holding one.
func cpuOnlyOnGPUNode(j Job, c Cluster, add func(Finding)) {
	// A job holding shards or MPS is using the GPU, it just does not hold a
	// whole one. Accusing it of blocking GPU work would be exactly backwards.
	if j.GPUsRequested > 0 || j.SharesGPU {
		return
	}
	for _, name := range j.Nodes {
		n, ok := c.Nodes[name]
		if ok && n.GPUs > 0 {
			add(Finding{JobID: j.ID, Rule: "cpu-only-on-gpu-node", Severity: Warn,
				Message: fmt.Sprintf("requests no GPUs but holds %s, which has %d - those GPUs are only usable by another job if enough of the node is left free",
					name, n.GPUs)})
			return
		}
	}
}

// exclusiveOverAllocation: --exclusive takes the whole node, so a smaller GRES
// request leaves the rest of its GPUs idle and unschedulable. Invisible in
// every aggregate, because the node reads as fully allocated.
//
// --exclusive=user and =mcs strand the same GPUs from everyone outside that
// scope. The message says which, because the fix differs: dropping the flag
// entirely, or narrowing it.
func exclusiveOverAllocation(j Job, c Cluster, add func(Finding)) {
	if !j.Exclusive || j.GPUsRequested <= 0 {
		return
	}
	scope := "no other job"
	switch j.ExclusiveMode {
	case "user":
		scope = "no job from another user"
	case "mcs":
		scope = "no job outside this MCS group"
	}
	for _, name := range j.Nodes {
		n, ok := c.Nodes[name]
		if !ok || n.GPUs <= j.GPUsRequested {
			continue
		}
		add(Finding{JobID: j.ID, Rule: "exclusive-over-allocation", Severity: Warn,
			Message: fmt.Sprintf("exclusive on %s (%d GPUs) but requested only %d - %s can use the other %d",
				name, n.GPUs, j.GPUsRequested, scope, n.GPUs-j.GPUsRequested)})
		return
	}
}

// cpuOverAllocation: the third way to strand a GPU without asking for it.
// Taking every core on a node leaves nothing for another job to run on, so
// the GPUs this one did not request cannot be scheduled either.
//
// Same shape as memoryOverAllocation and exclusiveOverAllocation, and the
// three are deliberately separate rules rather than one "you took the node":
// the fix differs. Memory wants --mem, cores want -c, exclusivity wants the
// flag removed.
func cpuOverAllocation(j Job, c Cluster, add func(Finding)) {
	if j.GPUsRequested <= 0 || j.CPUs <= 0 {
		return
	}
	for _, name := range j.Nodes {
		n, ok := c.Nodes[name]
		if !ok || n.CPUs <= 0 || n.GPUs <= j.GPUsRequested {
			continue
		}
		if j.CPUs < n.CPUs {
			continue
		}
		add(Finding{JobID: j.ID, Rule: "cpu-over-allocation", Severity: Warn,
			Message: fmt.Sprintf("holds all %d CPUs on %s while requesting %d of its %d GPUs - nothing else can run there, so the other %d cannot be scheduled",
				n.CPUs, name, j.GPUsRequested, n.GPUs, n.GPUs-j.GPUsRequested)})
		return
	}
}

// memoryOverAllocation: holding all of a node's memory strands every GPU the
// job did not ask for, because nothing else can be scheduled there. Same
// finding as exclusiveOverAllocation through a different resource, and a
// quieter one - the job never mentions memory, so nothing in the submission
// looks like a claim on the whole node.
//
// It reports what is held, not why. A job that named no memory and a job that
// deliberately asked for all of it produce the same unschedulable GPUs, and
// guessing between them would be guessing.
func memoryOverAllocation(j Job, c Cluster, add func(Finding)) {
	if j.GPUsRequested <= 0 || j.MemoryMB <= 0 {
		return
	}
	for _, name := range j.Nodes {
		n, ok := c.Nodes[name]
		if !ok || n.MemoryMB <= 0 || n.GPUs <= j.GPUsRequested {
			continue
		}
		if j.MemoryMB < n.MemoryMB {
			continue
		}
		add(Finding{JobID: j.ID, Rule: "memory-over-allocation", Severity: Warn,
			Message: fmt.Sprintf("holds all %d MB on %s while requesting %d of its %d GPUs - the other %d cannot be scheduled, because no job can fit alongside this one",
				n.MemoryMB, name, j.GPUsRequested, n.GPUs, n.GPUs-j.GPUsRequested)})
		return
	}
}
