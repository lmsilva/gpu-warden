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
// What they render is the wire shape - the published form of a pass, not the
// internal one. A renderer reading richer internal data would know things a
// consumer of the published document could not, and the difference would
// surface as two tools disagreeing about the same job. The renderers reading
// the published shape is what proves the shape publishes enough.
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

	"github.com/lmsilva/squire/internal/wire"
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
func rows(s wire.Snapshot, dollarRate float64) []row {
	out := make([]row, 0, len(s.Jobs))
	for _, j := range s.Jobs {
		out = append(out, toRow(j, dollarRate))
	}
	return out
}

// sizingWords is the one place the machine vocabulary becomes the table's
// phrasing. The wire says right_sized because the sizing metric already
// does, and two machine surfaces must not spell one enum two ways; how to
// say it to a person is this renderer's decision alone.
var sizingWords = map[string]string{
	"unknown":           "-",
	"right_sized":       "right-sized",
	"over_provisioned":  "over-provisioned",
	"under_provisioned": "possibly under-provisioned",
}

func sizingWord(code string) string {
	if w, ok := sizingWords[code]; ok {
		return w
	}
	// A state this renderer predates. The raw code is better than a dash
	// that would claim nothing was decided.
	return code
}

func toRow(j wire.Job, dollarRate float64) row {
	// Activity carries its confidence inline ("zombie:high"); a verdict
	// without one (analyzing) prints bare.
	activity := j.Activity
	if j.Confidence != nil {
		activity += ":" + *j.Confidence
	}
	mem := "-"
	if m := j.Memory; m != nil {
		mem = fmt.Sprintf("%.1f/%.0fG (%.0f%%)",
			m.PeakMiB/1024, m.CapacityMiB/1024, m.PeakFrac*100)
	}
	// Null on the wire is a dash on the row: a value that was never read
	// must not be mistakable for a value that was read as zero, and a
	// column of numbers cannot say "unknown" any other way.
	avg, peak, wasted := "-", "-", "-"
	if j.AvgUtilPct != nil {
		avg = fmt.Sprintf("%.0f", *j.AvgUtilPct)
	}
	if j.PeakUtilPct != nil {
		peak = fmt.Sprintf("%.0f", *j.PeakUtilPct)
	}
	if j.WastedGPUHours != nil {
		wasted = fmt.Sprintf("%.1f", *j.WastedGPUHours)
		if dollarRate > 0 {
			wasted = fmt.Sprintf("%.1f ($%.2f)",
				*j.WastedGPUHours, *j.WastedGPUHours*dollarRate)
		}
	}
	// A job whose telemetry could not be scoped to its own GPU devices is
	// marked, because on a shared node those numbers include a neighbour's
	// work. Silently printing them as if they were the job's own is the
	// failure this marker exists to prevent.
	gpus := strconv.Itoa(j.GPUs)
	if !j.PerGPU {
		gpus += "*"
	}
	lit := "-"
	if j.LitGPUs != nil {
		lit = strconv.Itoa(*j.LitGPUs)
	}
	firstWork := "-"
	if j.FirstWorkAfterSeconds != nil {
		firstWork = (time.Duration(*j.FirstWorkAfterSeconds) * time.Second).String()
	}
	return row{
		JobID:   j.ID,
		Name:    j.Name,
		User:    j.User,
		GPUs:    gpus,
		Elapsed: (time.Duration(j.ElapsedSeconds) * time.Second).Round(time.Minute).String(),
		Avg:     avg,
		Peak:    peak,
		Mem:     mem,
		Wasted:  wasted,
		// Every reason, not just the first. Judge appends them in priority
		// order: what the GPU is doing, then anything that argued with it,
		// then sizing. The first alone can assert over-provisioned and never
		// say on what evidence.
		Activity: activity, State: j.Activity, Sizing: sizingWord(j.Sizing),
		Lit: lit, FirstWork: firstWork, Why: strings.Join(j.Reasons, "; "),
	}
}

const (
	podWideNote = "* GPU indices unavailable (no gres_detail): telemetry covers every GPU on the job's nodes."
	faultedNote = "the GR_ENGINE_ACTIVE signal read flat cluster-wide while GPUs were busy,\n" +
		"so it was ignored this cycle. Verdicts stand; confidence is lower than it could be."
)

// Table renders a pass as a top-style table: one column per verdict axis,
// the findings under it, and any caveat that applies to the whole pass.
func Table(out io.Writer, s wire.Snapshot, o TableOptions) {
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
	if s.PodWide() {
		fmt.Fprintln(out, "\n"+podWideNote)
	}
	if s.EngineFaulted {
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
func findings(out io.Writer, s wire.Snapshot) {
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
