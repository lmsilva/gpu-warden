package view

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"time"

	"github.com/lmsilva/squire/internal/wire"
)

// HTMLOptions are the web renderer's settings.
//
// There is no Wide here. A terminal rations width; a browser does not, so the
// evidence behind every verdict is always shown.
type HTMLOptions struct {
	// DollarRate costs the wasted GPU-hours, exactly as it does in the table.
	DollarRate float64
	// Version is what the footer reports, so a screenshot names the build
	// that produced it.
	Version string
	// Refresh asks the browser to reload on this interval. Zero leaves the
	// page still.
	Refresh time.Duration
}

// page is everything the template renders. Building it here rather than
// calling methods from the template keeps the logic where it can be tested
// and the template to placement.
type page struct {
	Version       string
	At            string
	Refresh       int
	Rows          []row
	Allocation    []pageFinding
	Configuration []pageFinding
	Waiting       string
	PodWide       string
	Faulted       string
	ClusterUnread string
}

type pageFinding struct {
	JobID    int
	Name     string
	Severity string
	Rule     string
	Message  string
}

// html/template, not text/template. Job names and usernames come from Slurm
// and are whatever a user typed, so they are the injection: a job named with
// a script tag is a submission away. This package escapes on the way out
// rather than sanitising on the way in, because Squire must report the name
// the job actually has.
var pageTmpl = template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="{{.Refresh}}">{{end}}
<title>squire</title>
<style>
/* Colours are named once and redefined for a dark background, rather than
   picked to sit somewhere between the two. A mid grey that survives both is
   a mid grey that is hard to read on either. */
:root {
  color-scheme: light dark;
  --fg: #111;
  --muted: #55595e;
  --line: #d8dade;
  --zombie: #b3261e;
  --idle: #8a5300;
  --healthy: #146c2e;
}
@media (prefers-color-scheme: dark) {
  :root {
    --fg: #e9eaec;
    --muted: #a9adb3;
    --line: #3a3d42;
    --zombie: #ff8a80;
    --idle: #ffc046;
    --healthy: #6ee7a0;
  }
}
body { font: 14px/1.5 ui-sans-serif, system-ui, sans-serif; margin: 2rem auto;
       max-width: 90rem; padding: 0 1rem; color: var(--fg); }
h1 { font-size: 1.25rem; margin: 0; }
h2 { font-size: 1rem; margin: 2rem 0 .5rem; }
.meta { color: var(--muted); margin: .25rem 0 1.5rem; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .35rem .6rem; border-bottom: 1px solid var(--line);
         white-space: nowrap; }
th { font-weight: 600; font-size: .8rem; text-transform: uppercase;
     letter-spacing: .03em; color: var(--muted); }
td.num { text-align: right; font-variant-numeric: tabular-nums; }
/* The evidence line is secondary to its row but still has to be readable -
   it is the part that explains the verdict, so it gets the body colour at a
   smaller size rather than a lighter grey. */
tr.why td { border-bottom: 1px solid var(--line); padding-top: 0;
            white-space: normal; font-size: .9rem; }
tr.why + tr td { border-top: 0; }
.state-zombie { color: var(--zombie); font-weight: 600; }
.state-idle { color: var(--idle); font-weight: 600; }
.state-healthy { color: var(--healthy); }
.state-analyzing { color: var(--muted); }
.sev-warn { color: var(--zombie); font-weight: 600; }
.sev-note { color: var(--idle); }
.note { margin: 1rem 0; }
footer { color: var(--muted); margin-top: 2.5rem; font-size: .85rem; }
</style>
</head>
<body>
<h1>squire</h1>
<p class="meta">{{if .At}}as of {{.At}}. {{end}}{{.Waiting}}</p>

{{if .Rows}}
<table>
<thead><tr>
<th>Job</th><th>Name</th><th>User</th><th>GPUs</th><th>Elapsed</th>
<th>Avg%</th><th>Peak%</th><th>GPU memory</th><th>Wasted GPU-h</th>
<th>Activity</th><th>Sizing</th><th>Lit</th><th>First work</th>
</tr></thead>
<tbody>
{{range .Rows}}
<tr>
<td class="num">{{.JobID}}</td><td>{{.Name}}</td><td>{{.User}}</td>
<td class="num">{{.GPUs}}</td><td class="num">{{.Elapsed}}</td>
<td class="num">{{.Avg}}</td><td class="num">{{.Peak}}</td>
<td>{{.Mem}}</td><td class="num">{{.Wasted}}</td>
<td class="state-{{.State}}">{{.Activity}}</td><td>{{.Sizing}}</td>
<td class="num">{{.Lit}}</td><td class="num">{{.FirstWork}}</td>
</tr>
{{if .Why}}<tr class="why"><td colspan="13">{{.Why}}</td></tr>{{end}}
{{end}}
</tbody>
</table>
{{else}}
<p>No GPU jobs are running.</p>
<p class="meta">This table is GPU jobs only — every column in it is a card measurement, so a job holding no GPU would be a row of dashes. Those jobs are still checked: they appear under Configuration findings below when something is wrong with what they asked for.</p>
{{end}}

{{if .PodWide}}<p class="note">{{.PodWide}}</p>{{end}}
{{if .Faulted}}<p class="note">{{.Faulted}}</p>{{end}}
{{if .ClusterUnread}}<p class="note">{{.ClusterUnread}}</p>{{end}}

{{if .Allocation}}
<h2>Allocation findings</h2>
<p class="meta">What the cards did, for the jobs in the table above.</p>
<table>
<thead><tr><th>Job</th><th>Name</th><th>Severity</th><th>Rule</th><th>Finding</th></tr></thead>
<tbody>
{{range .Allocation}}
<tr>
<td class="num">{{.JobID}}</td><td>{{.Name}}</td>
<td class="sev-{{.Severity}}">{{.Severity}}</td><td>{{.Rule}}</td>
<td style="white-space: normal">{{.Message}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}

{{if .Configuration}}
<h2>Configuration findings</h2>
<p class="meta">What jobs asked for. These cover every job Slurm knows about, including jobs that hold no GPU and jobs that have not started — so they can name a job the table above does not show.</p>
<table>
<thead><tr><th>Job</th><th>Name</th><th>Severity</th><th>Rule</th><th>Finding</th></tr></thead>
<tbody>
{{range .Configuration}}
<tr>
<td class="num">{{.JobID}}</td><td>{{.Name}}</td>
<td class="sev-{{.Severity}}">{{.Severity}}</td><td>{{.Rule}}</td>
<td style="white-space: normal">{{.Message}}</td>
</tr>
{{end}}
</tbody>
</table>
{{end}}

<footer>squire {{.Version}}</footer>
</body>
</html>
`))

// HTML renders a pass as a web page.
//
// The empty cluster is the one place this deliberately says more than the
// terminal table does. A table with a header and no rows reads correctly in a
// shell; the same thing in a browser reads as a Squire that has lost its
// jobs. So the page says it in words - and says which jobs it is talking
// about, because "nothing is running" is confusing to somebody looking at a
// queue full of CPU work. The two presenters agree on every job; they differ
// on how to render the absence of any.
//
// It builds the whole document before writing a byte of it. A template that
// fails halfway through would otherwise leave a 200 already sent and half a
// page behind it, which reads as a Squire that has lost some jobs rather than
// as an error.
func HTML(w io.Writer, s wire.Snapshot, o HTMLOptions) error {
	var alloc, conf []pageFinding
	for _, f := range s.Findings {
		pf := pageFinding{
			JobID: f.JobID, Name: f.JobName,
			Severity: f.Severity, Rule: f.Rule, Message: f.Message,
		}
		// Two tables, because the two kinds are not the same claim. A kind
		// this page predates goes with the allocation findings rather than
		// vanishing.
		if f.Kind == wire.KindConfiguration {
			conf = append(conf, pf)
		} else {
			alloc = append(alloc, pf)
		}
	}

	p := page{
		Version:       o.Version,
		Refresh:       int(o.Refresh.Seconds()),
		Rows:          rows(s, o.DollarRate),
		Allocation:    alloc,
		Configuration: conf,
		Waiting:       waiting(s),
	}
	if !s.At.IsZero() {
		p.At = s.At.Format("15:04:05 MST")
	}
	if s.PodWide() {
		p.PodWide = podWideNote
	}
	if s.EngineFaulted {
		p.Faulted = "Note: " + faultedNote
	}
	if s.ClusterUnread {
		p.ClusterUnread = "Note: " + clusterUnreadNote
	}

	var buf bytes.Buffer
	if err := pageTmpl.Execute(&buf, p); err != nil {
		return err
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// waiting states the queue pressure either way. A dashboard needs to tell
// "nobody is waiting" from "this number is missing", and on a page the
// difference has to be said in words rather than implied by a zero.
func waiting(s wire.Snapshot) string {
	q := s.Queue
	if q.PendingGPUJobs == 0 {
		return "Nothing is waiting for a GPU."
	}
	return fmt.Sprintf("%s waiting for %s.",
		plural(q.PendingGPUJobs, "job", "jobs"),
		plural(q.PendingGPUs, "GPU", "GPUs"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
