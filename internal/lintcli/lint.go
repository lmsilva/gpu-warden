// Package lintcli is the configuration check: it adapts what slurmrestd
// returns into the pure engine's inputs, runs the checks, and prints them.
//
// It lives outside package main so that more than one entry point can reach
// it - the squire binary's lint subcommand and the standalone squire-lint
// binary - and so a future web presenter can call the same adapter. Its only
// dependencies are the Slurm client and the engine: no Kubernetes, no
// Prometheus, which is what lets squire-lint link without either.
package lintcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmcfg"
)

// parseConfig builds the configuration for a check. It registers only the two
// flags this mode uses, so the help text describes what the check does rather
// than everything the squire binary can do.
func parseConfig(name string, args []string) slurmcfg.Config {
	var c slurmcfg.Config
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	slurmcfg.Bind(fs, &c)
	fs.Parse(args)
	return c
}

// Exit codes. Separating "found problems" from "could not check" matters for
// anything scripting this: a broken token and a clean queue must not look the
// same.
// timeout bounds one check. Three reads of slurmrestd and no telemetry, so it
// is generous rather than tight.
const timeout = 30 * time.Second

const (
	exitClean    = 0
	exitFindings = 1
	exitError    = 2
)

// toLintJob adapts a slurmrestd job into the pure engine's input. The adapter
// lives here rather than in internal/lint so that package keeps importing
// nothing but fmt, sort and time.
func toLintJob(j slurmapi.Job) lint.Job {
	out := lint.Job{
		ID:            j.JobID,
		Name:          j.Name,
		User:          j.Owner(),
		Partition:     j.Partition,
		State:         j.BaseState(),
		GPUsRequested: slurmapi.GPUCount(j),
		Exclusive:     j.Exclusive(),
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

// toCluster builds the engine's view of the cluster.
func toCluster(nodes []slurmapi.Node, parts []slurmapi.Partition) *lint.Cluster {
	c := &lint.Cluster{
		Nodes:      make(map[string]lint.Node, len(nodes)),
		Partitions: make(map[string]lint.Partition, len(parts)),
	}
	for _, n := range nodes {
		c.Nodes[n.Name] = lint.Node{
			Name: n.Name, GPUs: gresGPUs(n.Gres), MemoryMB: n.RealMemory.Number,
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

// scope describes how much of the queue was looked at. Slurm keeps finished
// jobs in its response for MinJobAge and those are not checked, so a bare job
// count would not match what the findings were drawn from.
func scope(checked, total int) string {
	if checked == total {
		return fmt.Sprintf("%d jobs", total)
	}
	return fmt.Sprintf("%d of %d jobs (%d already finished)", checked, total, total-checked)
}

// runLint is the CLI presenter over the pure engine. It reads slurmrestd and
// nothing else - no Prometheus, no Kubernetes, no DCGM - which is why it works
// on a cluster that has installed none of them.
func runLint(ctx context.Context, sc *slurmapi.Client, out io.Writer) int {
	jobs, err := sc.ListJobs(ctx)
	if err != nil {
		fmt.Fprintln(out, "error:", err)
		return exitError
	}
	// Cluster data is optional: without it the spec-shaped rules still run,
	// and the node- and partition-aware ones stay silent rather than guess.
	var cluster *lint.Cluster
	nodes, nerr := sc.ListNodes(ctx)
	parts, perr := sc.ListPartitions(ctx)
	if nerr == nil && perr == nil {
		cluster = toCluster(nodes, parts)
	} else {
		fmt.Fprintln(out, "note: could not read nodes or partitions; running job-spec rules only")
	}

	lj := make([]lint.Job, 0, len(jobs))
	checked := 0
	for _, j := range jobs {
		l := toLintJob(j)
		if lint.Checkable(l.State) {
			checked++
		}
		lj = append(lj, l)
	}
	findings := lint.CheckAll(lj, cluster)
	if len(findings) == 0 {
		fmt.Fprintf(out, "checked %s, no findings\n", scope(checked, len(jobs)))
		return exitClean
	}

	names := make(map[int]string, len(jobs))
	for _, j := range jobs {
		names[j.JobID] = j.Name
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "JOBID\tNAME\tSEVERITY\tRULE\tFINDING")
	for _, f := range findings {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			f.JobID, names[f.JobID], f.Severity, f.Rule, f.Message)
	}
	w.Flush()
	fmt.Fprintf(out, "\n%d findings across %s\n", len(findings), scope(checked, len(jobs)))
	return exitFindings
}

// Main runs a configuration check and returns the process exit code. name is
// what the flag set calls itself in help and error output, so each entry point
// can name itself.
//
// It builds the one client a check needs. The monitoring path also builds
// Prometheus and Kubernetes clients, which a login node has no credentials
// for - that separation is the whole reason this runs where users are.
func Main(name string, args []string) int {
	c := parseConfig(name, args)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sc := slurmapi.NewClient(c.URL, c.Version, c.Token)
	return runLint(ctx, sc, os.Stdout)
}
