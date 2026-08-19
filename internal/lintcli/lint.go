// Package lintcli is the configuration check: it reads slurmrestd, runs the
// checks over what slurmfacts translated, and prints them.
//
// It lives outside package main so that more than one entry point can reach
// it - the squire binary's lint subcommand and the standalone squire-lint
// binary. Its only dependencies are the Slurm client, the translation and the
// engine: no Kubernetes, no Prometheus, which is what lets squire-lint link
// without either.
package lintcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmcfg"
	"github.com/lmsilva/squire/internal/slurmfacts"
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
		cluster = slurmfacts.Cluster(nodes, parts)
	} else {
		fmt.Fprintln(out, "note: could not read nodes or partitions; running job-spec rules only")
	}

	lj := make([]lint.Job, 0, len(jobs))
	checked := 0
	for _, j := range jobs {
		l := slurmfacts.Job(j)
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
