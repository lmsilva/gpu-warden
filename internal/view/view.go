// Package view renders a pass for a person, as a terminal table or as a web
// page.
//
// Both renderers live here because they share the formatting. Every number a
// reader sees - a memory fraction, a cost, a dash standing for "not measured"
// - is turned into text once, in rows below, and the two renderers only
// decide where to put the result. Written as two packages they would drift,
// and the first symptom would be a page and a table disagreeing about the
// same job in front of the person trying to explain it.
//
// Neither renderer judges anything. The verdicts and findings arrive already
// decided in the snapshot.
package view

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/verdict"
)

// TableOptions are the terminal renderer's settings.
type TableOptions struct {
	// Wide adds the evidence columns. A terminal has a width budget, so the
	// evidence is opt-in there.
	Wide bool
	// DollarRate costs the wasted GPU-hours. Zero leaves the hours alone
	// rather than printing $0.00 against every job.
	DollarRate float64
}

// row is one job with every value already rendered as text. Both renderers
// take these, which is what makes it impossible for them to format the same
// number two ways.
type row struct {
	JobID     int
	Name      string
	User      string
	GPUs      string // carries the * marker when telemetry was not per-device
	Elapsed   string
	Avg       string
	Peak      string
	Mem       string
	Wasted    string
	Activity  string // the state with its confidence, e.g. "zombie:high"
	State     string // the state alone, for styling
	Sizing    string
	Lit       string
	FirstWork string
	Why       string
}

// rows renders every job in the snapshot.
func rows(s cycle.Snapshot, dollarRate float64) []row {
	out := make([]row, 0, len(s.Reports))
	for _, r := range s.Reports {
		out = append(out, toRow(r, dollarRate))
	}
	return out
}

func toRow(r report.JobReport, dollarRate float64) row {
	v := r.Verdict

	// Activity carries its confidence inline ("zombie:high"); a verdict
	// without one (analyzing) prints bare.
	activity := v.Activity.String()
	if v.Confidence != verdict.ConfNone {
		activity += ":" + v.Confidence.String()
	}
	sizing := "-"
	if v.Sizing != verdict.SizingUnknown {
		sizing = v.Sizing.String()
	}
	mem := "-"
	if r.CapacityMiB > 0 {
		mem = fmt.Sprintf("%.1f/%.0fG (%.0f%%)",
			r.PeakMemMiB/1024, r.CapacityMiB/1024, r.PeakMemFrac*100)
	}
	wasted := fmt.Sprintf("%.1f", r.WastedH)
	if dollarRate > 0 {
		wasted = fmt.Sprintf("%.1f ($%.2f)", r.WastedH, r.WastedH*dollarRate)
	}
	// A job whose telemetry could not be scoped to its own GPU devices is
	// marked, because on a shared node those numbers include a neighbour's
	// work. Silently printing them as if they were the job's own is the
	// failure this marker exists to prevent.
	gpus := strconv.Itoa(r.GPUs)
	if !r.PerGPU {
		gpus += "*"
	}
	// A dash rather than a zero for both: an unmeasured signal and a
	// measured zero mean opposite things, and a column of numbers cannot
	// say "unknown".
	lit := "-"
	if r.HasLit {
		lit = strconv.Itoa(r.LitGPUs)
	}
	firstWork := "-"
	if r.HasFirstWork {
		firstWork = r.FirstWorkAfter.Round(time.Second).String()
	}
	return row{
		JobID:   r.Job.JobID,
		Name:    r.Job.Name,
		User:    r.Job.Owner(),
		GPUs:    gpus,
		Elapsed: r.Elapsed.Round(time.Minute).String(),
		Avg:     fmt.Sprintf("%.0f", r.AvgUtil),
		Peak:    fmt.Sprintf("%.0f", r.PeakUtil),
		Mem:     mem,
		Wasted:  wasted,
		// Every reason, not just the first. Judge appends them in priority
		// order: what the GPU is doing, then anything that argued with it,
		// then sizing. The first alone can assert over-provisioned and never
		// say on what evidence.
		Activity: activity, State: v.Activity.String(), Sizing: sizing,
		Lit: lit, FirstWork: firstWork, Why: strings.Join(v.Reasons, "; "),
	}
}

// podWide reports whether any job's telemetry covered more than its own
// devices, which is what the * marker in the GPUS column stands for.
func podWide(s cycle.Snapshot) bool {
	for _, r := range s.Reports {
		if !r.PerGPU {
			return true
		}
	}
	return false
}

// engineFaulted reports whether the engine signal was dropped this pass. It
// is a fleet-level judgement, so every report in a pass carries the same
// answer.
func engineFaulted(s cycle.Snapshot) bool {
	for _, r := range s.Reports {
		if r.EngineFaulted {
			return true
		}
	}
	return false
}

const (
	podWideNote = "* GPU indices unavailable (no gres_detail): telemetry covers every GPU on the job's nodes."
	faultedNote = "the GR_ENGINE_ACTIVE signal read flat cluster-wide while GPUs were busy,\n" +
		"so it was ignored this cycle. Verdicts stand; confidence is lower than it could be."
)

// Table renders a pass as a top-style table: one column per verdict axis,
// the findings under it, and any caveat that applies to the whole pass.
func Table(out io.Writer, s cycle.Snapshot, o TableOptions) {
	// tabwriter buffers: nothing prints until Flush.
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	header := "JOBID\tNAME\tUSER\tGPUS\tELAPSED\tAVG%\tPEAK%\tGPU-MEM\tWASTED-GPU-H\tACTIVITY\tSIZING"
	if o.Wide {
		// LIT and FIRST-WORK are measurements rather than judgements, which
		// is why they sit with the evidence rather than in the default table.
		// Without them the findings below can only be trusted, not checked.
		header += "\tLIT\tFIRST-WORK\tWHY"
	}
	fmt.Fprintln(w, header)
	for _, r := range rows(s, o.DollarRate) {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
			r.JobID, r.Name, r.User, r.GPUs, r.Elapsed, r.Avg, r.Peak,
			r.Mem, r.Wasted, r.Activity, r.Sizing)
		if o.Wide {
			fmt.Fprintf(w, "\t%s\t%s\t%s", r.Lit, r.FirstWork, r.Why)
		}
		fmt.Fprintln(w)
	}
	w.Flush()
	if podWide(s) {
		fmt.Fprintln(out, "\n"+podWideNote)
	}
	if engineFaulted(s) {
		fmt.Fprintln(out, "\nnote: "+faultedNote)
	}
	findings(out, s)
}

// findings writes the findings under the verdict table.
//
// A separate block rather than more columns: the table answers "what is every
// job doing", one row each, and findings are exceptions that most jobs do not
// have. Widening every row for something few of them carry would cost the
// table its shape. The columns match squire-lint's, so a reader who has seen
// one recognises the other.
func findings(out io.Writer, s cycle.Snapshot) {
	if len(s.Findings) == 0 {
		return
	}
	names := s.Names()
	fmt.Fprintln(out)
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "JOBID\tNAME\tSEVERITY\tRULE\tFINDING")
	for _, f := range s.Findings {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			f.JobID, names[f.JobID], f.Severity, f.Rule, f.Message)
	}
	w.Flush()
}
