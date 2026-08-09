package verdict

import (
	"testing"
	"time"
)

func TestJudge(t *testing.T) {
	th := DefaultThresholds()
	const GiB = 1024.0 // MiB in a GiB, since DCGM reports framebuffer in MiB

	cases := []struct {
		name     string
		m        Metrics
		activity Activity
		sizing   Sizing
	}{
		{
			name: "young job is still analyzing",
			m: Metrics{Elapsed: 5 * time.Minute, HasStart: true,
				HasUtil: true, AvgUtil: 3, PeakUtil: 3,
				HasMem: true, PeakMemFrac: 0.01, AvgMemFrac: 0.01},
			activity: Analyzing, sizing: SizingUnknown,
		},
		{
			name:     "no telemetry is analyzing, never a false all-clear",
			m:        Metrics{Elapsed: 3 * time.Hour, HasStart: true, HasUtil: false},
			activity: Analyzing, sizing: SizingUnknown,
		},
		{
			name: "early burst ends warmup, healthy",
			m: Metrics{Elapsed: 4 * time.Minute, HasStart: true,
				HasUtil: true, AvgUtil: 80, PeakUtil: 99,
				HasMem: true, PeakMemFrac: 0.70, AvgMemFrac: 0.60},
			activity: Healthy, sizing: RightSized,
		},
		{
			name: "busy and big enough is healthy and right-sized",
			m: Metrics{Elapsed: 90 * time.Minute, HasStart: true,
				HasUtil: true, AvgUtil: 95, PeakUtil: 100,
				HasMem: true, PeakMemFrac: 0.65, AvgMemFrac: 0.60},
			activity: Healthy, sizing: RightSized,
		},
		{
			// The guard on the over-provisioned assertion. Same tiny memory
			// footprint as the case below, but this job is working the card
			// hard - it is compute-bound and correctly sized, and asserting
			// over-provisioned on it would be a false accusation.
			name: "compute-bound with a small footprint is right-sized",
			m: Metrics{Elapsed: 3 * time.Hour, HasStart: true,
				HasUtil: true, AvgUtil: 96, PeakUtil: 99,
				HasMem: true, PeakMemFrac: 0.10, AvgMemFrac: 0.09,
				PeakMemMiB: 1.5 * GiB, CapacityMiB: 16.0 * GiB},
			activity: Healthy, sizing: RightSized,
		},
		{
			name: "healthy but tiny memory footprint is over-provisioned",
			m: Metrics{Elapsed: 3 * time.Hour, HasStart: true,
				HasUtil: true, AvgUtil: 35, PeakUtil: 100,
				HasMem: true, PeakMemFrac: 0.18, AvgMemFrac: 0.15,
				PeakMemMiB: 3.0 * GiB, CapacityMiB: 16.0 * GiB},
			activity: Healthy, sizing: OverProvisioned,
		},
		{
			name: "pinned and memory-full hints under-provisioned",
			m: Metrics{Elapsed: 3 * time.Hour, HasStart: true,
				HasUtil: true, AvgUtil: 99, PeakUtil: 100,
				HasMem: true, PeakMemFrac: 0.98, AvgMemFrac: 0.95},
			activity: Healthy, sizing: UnderProvisioned,
		},
		{
			name: "short idle that never worked is Idle, not yet Zombie",
			m: Metrics{Elapsed: 40 * time.Minute, HasStart: true,
				HasUtil: true, AvgUtil: 1, PeakUtil: 1,
				HasMem: true, PeakMemFrac: 0.20, AvgMemFrac: 0.20},
			activity: Idle, sizing: SizingUnknown,
		},
		{
			name: "long idle is a Zombie (duration)",
			m: Metrics{Elapsed: 3 * time.Hour, HasStart: true,
				HasUtil: true, AvgUtil: 0, PeakUtil: 0,
				HasMem: true, PeakMemFrac: 0.20, AvgMemFrac: 0.20},
			activity: Zombie, sizing: SizingUnknown,
		},
		{
			name: "worked then went flat is a Zombie (crash signature)",
			m: Metrics{Elapsed: 50 * time.Minute, HasStart: true,
				HasUtil: true, AvgUtil: 40, PeakUtil: 2,
				HasMem: true, PeakMemFrac: 0.50, AvgMemFrac: 0.40},
			activity: Zombie, sizing: SizingUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(tc.m, th)
			if v.Activity != tc.activity {
				t.Errorf("activity: got %s, want %s", v.Activity, tc.activity)
			}
			if v.Sizing != tc.sizing {
				t.Errorf("sizing: got %s, want %s", v.Sizing, tc.sizing)
			}
			t.Logf("%-52s -> %-28s :: %s", tc.name, v.Summary(), v.Reason())
		})
	}
}

// TestJudgeEngineConfidence covers the optional engine signals. They must never
// change WHAT Squire finds — only how confident it is and what it says — so
// every case below asserts the activity verdict is untouched.
func TestJudgeEngineConfidence(t *testing.T) {
	th := DefaultThresholds()

	// A busy-looking job. Utilization is pegged; the engine signals decide how
	// much that reading is worth.
	busy := Metrics{Elapsed: 3 * time.Hour, HasStart: true,
		HasUtil: true, AvgUtil: 95, PeakUtil: 100,
		HasMem: true, PeakMemFrac: 0.60, AvgMemFrac: 0.55}

	// A crash-signature zombie: worked earlier, flat now, not yet past the
	// 2h duration bar.
	crashed := Metrics{Elapsed: 50 * time.Minute, HasStart: true,
		HasUtil: true, AvgUtil: 40, PeakUtil: 2,
		HasMem: true, PeakMemFrac: 0.50, AvgMemFrac: 0.40}

	with := func(m Metrics, f func(*Metrics)) Metrics { f(&m); return m }

	cases := []struct {
		name     string
		m        Metrics
		activity Activity
		conf     Confidence
	}{
		{
			name:     "no profiling metrics: trust utilization",
			m:        busy,
			activity: Healthy, conf: ConfHigh,
		},
		{
			name: "engine lit and SMs busy: high confidence",
			m: with(busy, func(m *Metrics) {
				m.HasGrEngine, m.PeakGrEngine = true, 0.85
				m.HasSM, m.PeakSM = true, 0.70
			}),
			activity: Healthy, conf: ConfHigh,
		},
		{
			name: "engine lit but few SMs active: busy but shallow",
			m: with(busy, func(m *Metrics) {
				m.HasGrEngine, m.PeakGrEngine = true, 0.90
				m.HasSM, m.PeakSM = true, 0.08
			}),
			activity: Healthy, conf: ConfMedium,
		},
		{
			name: "engine unlit despite 100% util: uncorroborated, still Healthy",
			m: with(busy, func(m *Metrics) {
				m.HasGrEngine, m.PeakGrEngine = true, 0.01
				m.HasSM, m.PeakSM = true, 0.01
			}),
			activity: Healthy, conf: ConfLow,
		},
		{
			name:     "crash signature alone is medium confidence",
			m:        crashed,
			activity: Zombie, conf: ConfMedium,
		},
		{
			name: "crash signature corroborated by a flat engine is high",
			m: with(crashed, func(m *Metrics) {
				m.HasGrEngine, m.PeakGrEngine = true, 0.00
			}),
			activity: Zombie, conf: ConfHigh,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(tc.m, th)
			if v.Activity != tc.activity {
				t.Errorf("activity: got %s, want %s (enrichment must not change the finding)", v.Activity, tc.activity)
			}
			if v.Confidence != tc.conf {
				t.Errorf("confidence: got %s, want %s", v.Confidence, tc.conf)
			}
			t.Logf("%-52s -> %-22s :: %s", tc.name, v.Summary(), v.Reasons[len(v.Reasons)-1])
		})
	}
}

// TestPeakWindow pins the rule that a peak reading never reaches back past the
// job's own start. A job younger than the window that inherits its
// predecessor's utilization reads as healthy while doing nothing, which is the
// worst failure this tool has: a false all-clear.
func TestPeakWindow(t *testing.T) {
	cases := []struct {
		name    string
		elapsed time.Duration
		window  time.Duration
		want    time.Duration
	}{
		{"younger than the window: clamped to its own age", 10 * time.Minute, 30 * time.Minute, 10 * time.Minute},
		{"older than the window: the window stands", 3 * time.Hour, 30 * time.Minute, 30 * time.Minute},
		{"exactly the window", 30 * time.Minute, 30 * time.Minute, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := PeakWindow(c.elapsed, c.window); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
