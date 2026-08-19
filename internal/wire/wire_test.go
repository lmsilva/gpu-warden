package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/lint"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/verdict"
)

// measured is a report with every optional signal read.
func measured() report.JobReport {
	return report.JobReport{
		Job: slurmapi.Job{JobID: 101, Name: "train", UserName: "ana",
			UserID: slurmapi.NoVal{Set: true, Number: 50000}, Partition: "all"},
		GPUs:        2,
		PerGPU:      true,
		Elapsed:     90 * time.Minute,
		AvgUtil:     94,
		PeakUtil:    100,
		PeakMemMiB:  4096,
		CapacityMiB: 15095,
		PeakMemFrac: 0.27,
		WastedH:     0.2,
		HasData:     true,
		HasLit:      true, LitGPUs: 2,
		HasFirstWork: true, FirstWorkAfter: 58 * time.Second,
		Verdict: verdict.Verdict{
			Activity: verdict.Healthy, Sizing: verdict.RightSized,
			Confidence: verdict.ConfMedium,
			Reasons:    []string{"peak 100% over 30m0s"},
		},
	}
}

// unmeasured is a report where nothing optional was read.
func unmeasured() report.JobReport {
	return report.JobReport{
		Job:     slurmapi.Job{JobID: 102, Name: "wrap", UserName: "bo"},
		GPUs:    1,
		Elapsed: 2 * time.Minute,
		Verdict: verdict.Verdict{Activity: verdict.Analyzing},
	}
}

// TestPointersFollowTheMeasurement is the degradation contract on the wire:
// a value that was never read is null, and a value that was read is there
// with the number it was read as. This is the one thing about this package
// worth testing hardest, because a consumer summing a field must not be fed
// zeros that stand for "unknown".
func TestPointersFollowTheMeasurement(t *testing.T) {
	s := From(cycle.Snapshot{Reports: []report.JobReport{measured(), unmeasured()}})

	m := s.Jobs[0]
	if m.AvgUtilPct == nil || *m.AvgUtilPct != 94 {
		t.Errorf("measured avg must cross with its value, got %v", m.AvgUtilPct)
	}
	if m.PeakUtilPct == nil || *m.PeakUtilPct != 100 {
		t.Errorf("measured peak must cross with its value, got %v", m.PeakUtilPct)
	}
	if m.WastedGPUHours == nil || *m.WastedGPUHours != 0.2 {
		t.Errorf("measured waste must cross with its value, got %v", m.WastedGPUHours)
	}
	if m.Memory == nil || m.Memory.CapacityMiB != 15095 {
		t.Errorf("a read framebuffer must cross whole, got %+v", m.Memory)
	}
	if m.LitGPUs == nil || *m.LitGPUs != 2 {
		t.Errorf("a read lit count must cross, got %v", m.LitGPUs)
	}
	if m.FirstWorkAfterSeconds == nil || *m.FirstWorkAfterSeconds != 58 {
		t.Errorf("a read first-work must cross in seconds, got %v", m.FirstWorkAfterSeconds)
	}
	if m.UID == nil || *m.UID != 50000 {
		t.Errorf("a sent uid must cross, got %v", m.UID)
	}
	if m.Confidence == nil || *m.Confidence != "medium" {
		t.Errorf("a held confidence must cross as its word, got %v", m.Confidence)
	}

	u := s.Jobs[1]
	for name, got := range map[string]any{
		"avg_util_pct":     u.AvgUtilPct,
		"peak_util_pct":    u.PeakUtilPct,
		"wasted_gpu_hours": u.WastedGPUHours,
		"lit_gpus":         u.LitGPUs,
		"first_work":       u.FirstWorkAfterSeconds,
		"memory":           u.Memory,
		"uid":              u.UID,
		"confidence":       u.Confidence,
	} {
		// Typed nils inside any compare unequal to nil, so render and check.
		if b, _ := json.Marshal(got); string(b) != "null" {
			t.Errorf("unread %s must be null, got %s", name, b)
		}
	}
}

// TestVocabularyMatchesTheMetrics pins the wire's enum words to the metric
// labels' words, state by state. Two machine-readable surfaces describing
// the same enum in different spellings is a fork somebody's script falls
// into, so the agreement is tested rather than remembered.
func TestVocabularyMatchesTheMetrics(t *testing.T) {
	for _, a := range []verdict.Activity{verdict.Analyzing, verdict.Healthy,
		verdict.Idle, verdict.Zombie} {
		r := unmeasured()
		r.Verdict.Activity = a
		if got := fromReport(r).Activity; got != a.String() {
			t.Errorf("activity %v crossed as %q, metric says %q", a, got, a.String())
		}
	}
	for _, sz := range []verdict.Sizing{verdict.SizingUnknown, verdict.RightSized,
		verdict.OverProvisioned, verdict.UnderProvisioned} {
		r := unmeasured()
		r.Verdict.Sizing = sz
		if got := fromReport(r).Sizing; got != sz.Code() {
			t.Errorf("sizing %v crossed as %q, metric says %q", sz, got, sz.Code())
		}
	}
}

// TestEngineFaultIsSaidOnce: the flag is fleet-level and every internal
// report carries an identical copy, so the document says it once about the
// pass rather than once per job.
func TestEngineFaultIsSaidOnce(t *testing.T) {
	ok := measured()
	bad := measured()
	bad.EngineFaulted = true

	if s := From(cycle.Snapshot{Reports: []report.JobReport{ok}}); s.EngineFaulted {
		t.Error("a clean pass must not claim a fault")
	}
	if s := From(cycle.Snapshot{Reports: []report.JobReport{ok, bad}}); !s.EngineFaulted {
		t.Error("a faulted pass must say so at the top")
	}
}

// TestEmptyIsEmptyNotNull: an idle cluster is a valid document with empty
// lists, and a job with no reasons carries an empty list. null where a list
// belongs makes every consumer write a null check for a case Go callers
// never had.
func TestEmptyIsEmptyNotNull(t *testing.T) {
	b, err := json.Marshal(From(cycle.Snapshot{}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"jobs":[]`, `"findings":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("an empty pass must carry %s:\n%s", want, b)
		}
	}

	b, err = json.Marshal(From(cycle.Snapshot{Reports: []report.JobReport{unmeasured()}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"reasons":[]`) {
		t.Errorf("a job with no reasons must carry an empty list:\n%s", b)
	}
}

// TestFindingsCrossWithTheirWords: severity is its word, matching the column
// squire-lint prints.
func TestFindingsCrossWithTheirWords(t *testing.T) {
	s := From(cycle.Snapshot{Findings: []lint.Finding{
		{JobID: 7, Rule: "gpu-requested-never-touched", Severity: lint.Warn, Message: "m"},
		{JobID: 8, Rule: "slow-first-gpu-work", Severity: lint.Note, Message: "n"},
	}})
	if s.Findings[0].Severity != "warn" || s.Findings[1].Severity != "note" {
		t.Errorf("severities must cross as their words, got %+v", s.Findings)
	}
}

// TestNames mirrors the lookup the presenters need: findings carry an id and
// no name.
func TestNames(t *testing.T) {
	s := From(cycle.Snapshot{Reports: []report.JobReport{measured(), unmeasured()}})
	names := s.Names()
	if names[101] != "train" || names[102] != "wrap" {
		t.Errorf("names lookup wrong: %v", names)
	}
}

// TestPodWide: one under-scoped job marks the pass.
func TestPodWide(t *testing.T) {
	if From(cycle.Snapshot{Reports: []report.JobReport{measured()}}).PodWide() {
		t.Error("per-device telemetry must not mark the pass")
	}
	if !From(cycle.Snapshot{Reports: []report.JobReport{measured(), unmeasured()}}).PodWide() {
		t.Error("one node-wide job must mark the pass")
	}
}

// TestForUIDKeepsOnlyTheOwner: the filter matches the numeric uid, never the
// display name, and a job with no uid matches nobody - better to hide a job
// from its owner than to show it to someone else.
func TestForUIDKeepsOnlyTheOwner(t *testing.T) {
	mine := measured()
	theirs := measured()
	theirs.Job.JobID = 200
	theirs.Job.UserID = slurmapi.NoVal{Set: true, Number: 50001}
	nobody := unmeasured() // no uid at all

	s := From(cycle.Snapshot{Reports: []report.JobReport{mine, theirs, nobody}})
	got := s.ForUID(50000)
	if len(got.Jobs) != 1 || got.Jobs[0].ID != 101 {
		t.Fatalf("want job 101 alone, got %+v", got.Jobs)
	}

	// uid 0 is root, a legitimate owner - absent is what never matches.
	if got := s.ForUID(0); len(got.Jobs) != 0 {
		t.Errorf("no job here is root's, got %+v", got.Jobs)
	}
}

// TestForUIDFollowsTheFindings: a finding about a filtered-out job would name
// a job the document does not show.
func TestForUIDFollowsTheFindings(t *testing.T) {
	mine := measured()
	theirs := measured()
	theirs.Job.JobID = 200
	theirs.Job.UserID = slurmapi.NoVal{Set: true, Number: 50001}

	s := From(cycle.Snapshot{
		Reports: []report.JobReport{mine, theirs},
		Findings: []lint.Finding{
			{JobID: 101, Rule: "a", Severity: lint.Note, Message: "kept"},
			{JobID: 200, Rule: "b", Severity: lint.Warn, Message: "dropped"},
		},
	})
	got := s.ForUID(50000)
	if len(got.Findings) != 1 || got.Findings[0].JobID != 101 {
		t.Errorf("want the finding about job 101 alone, got %+v", got.Findings)
	}
}

// TestForUIDKeepsTheClusterFacts: the queue, the timestamp and the engine
// caveat are about the pass, so filtering to one owner leaves them alone -
// and an owner with nothing running still gets a valid document with empty
// lists.
func TestForUIDKeepsTheClusterFacts(t *testing.T) {
	bad := measured()
	bad.EngineFaulted = true
	s := From(cycle.Snapshot{
		Reports: []report.JobReport{bad},
		Queue:   report.Queue{PendingGPUJobs: 2, PendingGPUs: 9},
		At:      time.Date(2026, 8, 18, 14, 5, 9, 0, time.UTC),
	})

	got := s.ForUID(99999)
	if got.Queue != s.Queue || !got.At.Equal(s.At) || !got.EngineFaulted || got.Schema != Schema {
		t.Errorf("cluster facts must survive the filter: %+v", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"jobs":[]`, `"findings":[]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("an emptied pass must still carry %s:\n%s", want, b)
		}
	}
}
