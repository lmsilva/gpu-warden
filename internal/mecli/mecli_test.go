package mecli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
	"github.com/lmsilva/squire/internal/wire"
)

// TestResolveUID pins the precedence: --uid, then --user, then whoever is
// asking. Root's 0 is a value, both flags at once is an error, and a name
// that does not resolve points at the flag that always works.
func TestResolveUID(t *testing.T) {
	if got, err := resolveUID(config{uidSet: true, uid: 0, user: ""}); err != nil || got != 0 {
		t.Errorf("--uid 0 is root, got (%d, %v)", got, err)
	}
	if got, err := resolveUID(config{uidSet: true, uid: 50000}); err != nil || got != 50000 {
		t.Errorf("--uid must win, got (%d, %v)", got, err)
	}
	if _, err := resolveUID(config{uidSet: true, uid: -1}); err == nil {
		t.Error("a negative uid must be rejected")
	}
	if _, err := resolveUID(config{uidSet: true, uid: 1, user: "ana"}); err == nil {
		t.Error("--uid and --user together must be an error, not a pick")
	}
	if _, err := resolveUID(config{user: "no-such-user-here"}); err == nil ||
		!strings.Contains(err.Error(), "--uid") {
		t.Errorf("an unresolvable name must point at --uid, got %v", err)
	}
	if got, err := resolveUID(config{user: "root"}); err != nil || got != 0 {
		t.Errorf("--user root resolves to 0 everywhere, got (%d, %v)", got, err)
	}
	if got, err := resolveUID(config{}); err != nil || got != os.Geteuid() {
		t.Errorf("no flag means the invoking user, got (%d, %v)", got, err)
	}
}

// pass is a cluster with two owners: ana (50000) with a finding against her
// job, and bo (50001) running clean.
func pass() cycle.Snapshot {
	return cycle.Snapshot{
		Reports: []report.JobReport{
			{
				Job: slurmapi.Job{JobID: 101, Name: "train", UserName: "ana",
					UserID: slurmapi.NoVal{Set: true, Number: 50000}, Partition: "all"},
				GPUs: 2, PerGPU: true, Elapsed: 90 * time.Minute,
				Verdict: verdict.Verdict{Activity: verdict.Idle,
					Confidence: verdict.ConfHigh, Reasons: []string{"peak 4% over 30m0s"}},
			},
			{
				Job: slurmapi.Job{JobID: 200, Name: "infer", UserName: "bo",
					UserID: slurmapi.NoVal{Set: true, Number: 50001}, Partition: "all"},
				GPUs: 1, PerGPU: true, Elapsed: 10 * time.Minute,
				Verdict: verdict.Verdict{Activity: verdict.Healthy,
					Confidence: verdict.ConfMedium, Reasons: []string{"peak 96% over 10m0s"}},
			},
		},
		Findings: []lint.Finding{{JobID: 101, Rule: "gpu-requested-never-touched",
			Severity: lint.Warn, Message: "its 2 GPUs have done no work"}},
		// The job list every real pass carries. Findings are matched to an
		// owner through it, so a fixture without it cannot exercise the
		// owner filter honestly.
		Jobs: []slurmapi.Job{
			{JobID: 101, Name: "train", UserName: "ana",
				UserID: slurmapi.NoVal{Set: true, Number: 50000}},
			{JobID: 200, Name: "infer", UserName: "bo",
				UserID: slurmapi.NoVal{Set: true, Number: 50001}},
		},
		Queue: report.Queue{PendingGPUJobs: 1, PendingGPUs: 1},
		At:    time.Date(2026, 8, 18, 14, 5, 9, 0, time.UTC),
	}
}

// serve stands in for squire --serve: it filters and encodes exactly as the
// real route does, and records the uid each request asked for.
func serve(t *testing.T, asked *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.URL.Query().Get("uid"))
		s := wire.From(pass())
		if raw := r.URL.Query().Get("uid"); raw != "" {
			uid, err := strconv.Atoi(raw)
			if err != nil {
				http.Error(w, "bad uid", http.StatusBadRequest)
				return
			}
			s = s.ForUID(uid)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := wire.Encode(w, s); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
}

// TestRunRendersOneOwner is the round trip: ask for ana, get ana's table and
// the findings exit; ask for bo, get a clean 0; and the uid asked on the
// command line is the uid that reaches the server.
func TestRunRendersOneOwner(t *testing.T) {
	var asked []string
	srv := serve(t, &asked)
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := run(config{url: srv.URL}, 50000, &out, &errOut)
	if code != exitFindings {
		t.Errorf("ana has a finding, want exit %d, got %d\n%s", exitFindings, code, errOut.String())
	}
	for _, want := range []string{"JOBID", "101", "train", "idle:high", "SEVERITY", "warn"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ana's table is missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "infer") {
		t.Errorf("bo's job must not be in ana's table:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("a clean ask must write nothing to stderr: %s", errOut.String())
	}

	out.Reset()
	if code := run(config{url: srv.URL}, 50001, &out, &errOut); code != exitClean {
		t.Errorf("bo runs clean, want exit %d, got %d", exitClean, code)
	}
	if asked[0] != "50000" || asked[1] != "50001" {
		t.Errorf("the uid on the command line must reach the server, saw %v", asked)
	}
}

// TestRunJSONIsTheServersBytes: --json is the document as served, plus a
// trailing newline, with nothing else on stdout - a jq downstream sees what
// the server said.
func TestRunJSONIsTheServersBytes(t *testing.T) {
	var asked []string
	srv := serve(t, &asked)
	defer srv.Close()

	var want bytes.Buffer
	if err := wire.Encode(&want, wire.From(pass()).ForUID(50000)); err != nil {
		t.Fatal(err)
	}
	want.WriteByte('\n')

	var out, errOut bytes.Buffer
	code := run(config{url: srv.URL, jsonOut: true}, 50000, &out, &errOut)
	if code != exitFindings {
		t.Errorf("findings still set the exit in --json, got %d", code)
	}
	if out.String() != want.String() {
		t.Errorf("--json must be the server's bytes:\nwant %s\ngot  %s", want.String(), out.String())
	}
}

// TestRunRefusesAForeignSchema: a document under another contract exits 2
// before a byte reaches stdout, in both modes - a table quietly missing
// fields and a script parsing under the wrong contract are the same failure.
func TestRunRefusesAForeignSchema(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"schema":99,"jobs":[],"findings":[]}`))
	}))
	defer srv.Close()

	for _, jsonOut := range []bool{false, true} {
		var out, errOut bytes.Buffer
		if code := run(config{url: srv.URL, jsonOut: jsonOut}, 0, &out, &errOut); code != exitError {
			t.Errorf("jsonOut=%v: want exit %d, got %d", jsonOut, exitError, code)
		}
		if out.Len() != 0 {
			t.Errorf("jsonOut=%v: nothing may reach stdout: %s", jsonOut, out.String())
		}
		for _, want := range []string{"99", "1"} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("jsonOut=%v: the error must name both schemas: %s", jsonOut, errOut.String())
			}
		}
	}
}

// TestRunReportsTheServersError: a 500 carries squire's own sentence, and it
// lands on stderr with exit 2 rather than being parsed as a document.
func TestRunReportsTheServersError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "building reports: slurmrestd unreachable", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	if code := run(config{url: srv.URL}, 0, &out, &errOut); code != exitError {
		t.Errorf("want exit %d, got %d", exitError, code)
	}
	if !strings.Contains(errOut.String(), "building reports: slurmrestd unreachable") {
		t.Errorf("the server's sentence must survive: %s", errOut.String())
	}

	// And a squire that is not there at all is the same exit, said plainly.
	srv.Close()
	errOut.Reset()
	if code := run(config{url: srv.URL}, 0, &out, &errOut); code != exitError {
		t.Errorf("a dead server must exit %d, got %d", exitError, code)
	}
}

// TestFiltersReachTheServer: the client asks for less rather than receiving
// everything and hiding some of it. That is the same rule the owner filter
// follows, and it is what lets an authenticated server enforce the narrowing
// it already applies.
func TestFiltersReachTheServer(t *testing.T) {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.RawQuery)
		s := wire.From(pass())
		if raw := r.URL.Query().Get("uid"); raw != "" {
			uid, _ := strconv.Atoi(raw)
			s = s.ForUID(uid)
		}
		jobID, _ := strconv.Atoi(r.URL.Query().Get("job"))
		s = s.FilterFindings(wire.FindingFilter{Rule: r.URL.Query().Get("rule"), JobID: jobID})
		w.Header().Set("Content-Type", "application/json")
		if err := wire.Encode(w, s); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer srv.Close()

	var out, errOut bytes.Buffer
	code := run(config{url: srv.URL, rule: "no-such-rule"}, 50000, &out, &errOut)
	if code != exitClean {
		t.Errorf("a filter that matches nothing is clean, got %d: %s", code, errOut.String())
	}
	if !strings.Contains(asked[0], "rule=no-such-rule") || !strings.Contains(asked[0], "uid=50000") {
		t.Errorf("both terms must reach the server, saw %q", asked[0])
	}
	// The table still shows the owner's jobs - filtering findings must not
	// empty the table.
	if !strings.Contains(out.String(), "train") {
		t.Errorf("the job table stays whole:\n%s", out.String())
	}

	out.Reset()
	if code := run(config{url: srv.URL, job: 101}, 50000, &out, &errOut); code != exitFindings {
		t.Errorf("job 101 has a finding, want exit %d, got %d", exitFindings, code)
	}
	if !strings.Contains(asked[1], "job=101") {
		t.Errorf("the job term must reach the server, saw %q", asked[1])
	}
}
