package lintcli

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmcfg"
)

// TestFlagsAreCheckOnly pins the split. Sharing the monitoring flag set would
// put thresholds, Prometheus and Kubernetes settings in this mode's help, none
// of which it reads - and a flag a mode ignores is worse than a missing one,
// because it looks like it works.
//
// The list is exact rather than a minimum, so adding a flag to the shared
// binder for the monitoring path's benefit fails here and has to be a
// decision rather than an accident.
func TestFlagsAreCheckOnly(t *testing.T) {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	var c slurmcfg.Config
	slurmcfg.Bind(fs, &c)

	var got []string
	fs.VisitAll(func(f *flag.Flag) { got = append(got, f.Name) })
	want := map[string]bool{"slurm-url": true, "slurm-api": true, "slurm-token-file": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("the check registers %q, which it does not read", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the check should register %d flags, got %v", len(want), got)
	}
}

// fakeSlurm serves canned slurmrestd responses. jobs is the body for /jobs;
// an empty body for either collection makes that endpoint fail, which is how
// the degraded and error paths are exercised.
func fakeSlurm(t *testing.T, jobs, nodes, parts string) *slurmapi.Client {
	t.Helper()
	body := map[string]string{
		"/slurm/v0.0.44/jobs":       jobs,
		"/slurm/v0.0.44/nodes":      nodes,
		"/slurm/v0.0.44/partitions": parts,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := body[r.URL.Path]
		if !ok || b == "" {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(b))
	}))
	t.Cleanup(srv.Close)
	return slurmapi.NewClient(srv.URL, "v0.0.44", slurmapi.StaticToken("token"))
}

const (
	oneCleanJob = `{"jobs":[{"job_id":1,"name":"ok","partition":"batch",
		"job_state":["RUNNING"],"nodes":"a","tres_per_node":"gres/gpu:1",
		"time_limit":{"set":true,"number":60}}]}`
	oneBadJob = `{"jobs":[{"job_id":2,"name":"nolimit","partition":"batch",
		"job_state":["RUNNING"],"nodes":"a","tres_per_node":"gres/gpu:1"}]}`
	oneFinishedBadJob = `{"jobs":[{"job_id":3,"name":"done","partition":"batch",
		"job_state":["COMPLETED"],"nodes":"a"}]}`
	someNodes = `{"nodes":[{"name":"a","gres":"gpu:1","partitions":["batch"]}]}`
	someParts = `{"partitions":[{"name":"batch","maximums":{"time":{"set":true,"number":1440}}}]}`
)

// TestExitCodes is the contract anything scripting this depends on, and the
// one part of the output a script cannot see.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		jobs       string
		nodes      string
		parts      string
		want       int
		wantOutput string
	}{
		{name: "clean queue", jobs: oneCleanJob, nodes: someNodes, parts: someParts,
			want: exitClean, wantOutput: "no findings"},
		{name: "findings", jobs: oneBadJob, nodes: someNodes, parts: someParts,
			want: exitFindings, wantOutput: "no-time-limit"},
		{name: "slurmrestd unreachable", jobs: "", nodes: someNodes, parts: someParts,
			want: exitError, wantOutput: "error:"},
		{name: "cluster data missing still runs spec rules", jobs: oneBadJob, nodes: "", parts: "",
			want: exitFindings, wantOutput: "job-spec rules only"},
		// A finished job is still in Slurm's response for MinJobAge. Judging it
		// would accuse someone over an allocation they already gave back.
		{name: "finished job is not judged", jobs: oneFinishedBadJob, nodes: someNodes, parts: someParts,
			want: exitClean, wantOutput: "0 of 1 jobs"},
	}
	for _, tc := range cases {
		var sb strings.Builder
		got := runLint(context.Background(), fakeSlurm(t, tc.jobs, tc.nodes, tc.parts), &sb)
		if got != tc.want {
			t.Errorf("%s: exit %d, want %d (output: %s)", tc.name, got, tc.want, sb.String())
		}
		if !strings.Contains(sb.String(), tc.wantOutput) {
			t.Errorf("%s: output %q does not contain %q", tc.name, sb.String(), tc.wantOutput)
		}
	}
}
