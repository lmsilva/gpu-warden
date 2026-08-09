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
