package view

import (
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
)

func render(t *testing.T, s cycle.Snapshot, o HTMLOptions) string {
	t.Helper()
	var sb strings.Builder
	if err := HTML(&sb, s, o); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	return sb.String()
}

// TestHTMLEscapesJobNames is the reason this uses html/template. A job name
// is whatever a user typed into --job-name, so it reaches the page from
// outside and a submission is all it takes.
func TestHTMLEscapesJobNames(t *testing.T) {
	r := busy()
	r.Job.Name = `<script>alert(1)</script>`
	r.Job.UserName = `bo"><b>`
	out := render(t, cycle.Snapshot{Reports: []report.JobReport{r}}, HTMLOptions{})

	if strings.Contains(out, "<script>") {
		t.Errorf("a job name reached the page unescaped:\n%s", out)
	}
	if !strings.Contains(out, "&lt;script&gt;") {
		t.Errorf("the job name must still be shown, escaped:\n%s", out)
	}
	if strings.Contains(out, `bo"><b>`) {
		t.Errorf("a username reached the page unescaped:\n%s", out)
	}
}

// TestHTMLAndTableAgree is the whole argument for one package. Both
// renderers take the same rows, so a value that differs between them means
// the formatting has been forked.
func TestHTMLAndTableAgree(t *testing.T) {
	s := cycle.Snapshot{Reports: []report.JobReport{busy(), unmeasured()}}

	var tbl strings.Builder
	Table(&tbl, s, TableOptions{Wide: true, DollarRate: 0.53})
	page := render(t, s, HTMLOptions{DollarRate: 0.53})

	for _, want := range []string{"4.0/15G (27%)", "0.2 ($0.11)", "healthy:medium",
		"right-sized", "58s", "1*"} {
		if !strings.Contains(tbl.String(), want) {
			t.Errorf("table is missing %q", want)
		}
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

// TestHTMLAlwaysShowsTheEvidence pins the one deliberate difference: --wide
// does not exist here, because a browser has the width the flag was
// rationing.
func TestHTMLAlwaysShowsTheEvidence(t *testing.T) {
	out := render(t, cycle.Snapshot{Reports: []report.JobReport{busy()}}, HTMLOptions{})
	for _, want := range []string{"First work", "peak 100% over 30m0s", "avg 94% since start"} {
		if !strings.Contains(out, want) {
			t.Errorf("the page is missing %q:\n%s", want, out)
		}
	}
}

// TestHTMLEmptyCluster keeps an idle cluster from rendering an empty table,
// which reads as a Squire that has lost its jobs.
func TestHTMLEmptyCluster(t *testing.T) {
	out := render(t, cycle.Snapshot{}, HTMLOptions{})
	if !strings.Contains(out, "No GPU jobs are running") {
		t.Errorf("an idle cluster needs saying in words:\n%s", out)
	}
	if strings.Contains(out, "<tbody>") {
		t.Errorf("no jobs must mean no table:\n%s", out)
	}
	// And it must say which jobs it means. A cluster busy with CPU work
	// would otherwise read this as Squire having lost track of everything.
	if !strings.Contains(out, "CPU-only jobs are not shown") {
		t.Errorf("the empty state must name what it excludes:\n%s", out)
	}
}

// TestHTMLQueueIsStatedEitherWay covers both halves: a reader has to be able
// to tell "nobody is waiting" from a number that was never rendered.
func TestHTMLQueueIsStatedEitherWay(t *testing.T) {
	quiet := render(t, cycle.Snapshot{}, HTMLOptions{})
	if !strings.Contains(quiet, "Nothing is waiting for a GPU") {
		t.Errorf("an empty queue must be stated:\n%s", quiet)
	}

	busyQ := render(t, cycle.Snapshot{
		Queue: report.Queue{PendingGPUJobs: 2, PendingGPUs: 9},
	}, HTMLOptions{})
	// 2 jobs waiting for 9 GPUs, not 9 jobs waiting for 2.
	if !strings.Contains(busyQ, "2 jobs waiting for 9 GPUs") {
		t.Errorf("queue numbers transposed or reworded:\n%s", busyQ)
	}

	one := render(t, cycle.Snapshot{
		Queue: report.Queue{PendingGPUJobs: 1, PendingGPUs: 1},
	}, HTMLOptions{})
	if !strings.Contains(one, "1 job waiting for 1 GPU.") {
		t.Errorf("a single waiter must not read as plural:\n%s", one)
	}
}

func TestHTMLFindingsAndNotes(t *testing.T) {
	s := cycle.Snapshot{
		Reports: []report.JobReport{unmeasured()},
		Findings: []lint.Finding{{
			JobID: 102, Rule: "gpu-requested-never-touched", Severity: lint.Warn,
			Message: "its 1 GPU has done no work",
		}},
	}
	out := render(t, s, HTMLOptions{})
	// The pod-wide note is matched without its apostrophe: html/template
	// escapes the page's own text too, so "job's" arrives as "job&#39;s".
	for _, want := range []string{"Findings", "gpu-requested-never-touched", "warn",
		"wrap", "telemetry covers every GPU"} {
		if !strings.Contains(out, want) {
			t.Errorf("the page is missing %q:\n%s", want, out)
		}
	}
}

// TestHTMLRefreshIsOptional keeps a still page still. A meta refresh nobody
// asked for would reload a page somebody is reading.
func TestHTMLRefreshIsOptional(t *testing.T) {
	if out := render(t, cycle.Snapshot{}, HTMLOptions{}); strings.Contains(out, "http-equiv") {
		t.Errorf("no interval must mean no refresh:\n%s", out)
	}
	out := render(t, cycle.Snapshot{}, HTMLOptions{Refresh: 45 * time.Second})
	if !strings.Contains(out, `content="45"`) {
		t.Errorf("want a 45 second refresh:\n%s", out)
	}
}

// TestHTMLShowsTheBuildTime pins what "as of" means. The page can be served
// from cache for as long as --serve-cache, so the time it shows has to be
// when the numbers were taken rather than when the page was requested.
func TestHTMLShowsTheBuildTime(t *testing.T) {
	at := time.Date(2026, 8, 16, 14, 5, 9, 0, time.UTC)
	out := render(t, cycle.Snapshot{
		At:      at,
		Reports: []report.JobReport{{Job: slurmapi.Job{JobID: 1}}},
	}, HTMLOptions{})
	if !strings.Contains(out, "as of 14:05:09 UTC") {
		t.Errorf("want the build time on the page:\n%s", out)
	}

	// A snapshot nothing stamped says nothing, rather than 1970.
	if out := render(t, cycle.Snapshot{}, HTMLOptions{}); strings.Contains(out, "as of") {
		t.Errorf("an unstamped snapshot must not claim a time:\n%s", out)
	}
}

// TestHTMLFooterCarriesTheVersion. The footer is what a screenshot or a
// pasted bug report carries, so the build that produced the page has to be
// on it - and nothing else does.
func TestHTMLFooterCarriesTheVersion(t *testing.T) {
	out := render(t, cycle.Snapshot{}, HTMLOptions{Version: "v0.1.0"})
	if !strings.Contains(out, "<footer>squire v0.1.0</footer>") {
		t.Errorf("want the version alone in the footer:\n%s", out)
	}
}

// TestHTMLColoursAreNamedOnce guards the readability fix. Every colour is
// declared once as a variable, with a dark-background override; a hex literal
// in an ordinary rule is one that was chosen against one background and will
// be unreadable on the other.
func TestHTMLColoursAreNamedOnce(t *testing.T) {
	out := render(t, cycle.Snapshot{}, HTMLOptions{})
	if !strings.Contains(out, "prefers-color-scheme: dark") {
		t.Error("the page must define its colours for a dark background too")
	}
	style := out[strings.Index(out, "<style>"):strings.Index(out, "</style>")]
	for _, line := range strings.Split(style, "\n") {
		if strings.Contains(line, "#") && !strings.Contains(line, "--") {
			t.Errorf("colour literal outside the variable block: %s", strings.TrimSpace(line))
		}
	}
}
