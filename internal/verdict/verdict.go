// Package verdict turns a job's GPU telemetry into a two-axis efficiency
// judgement: what the GPU is doing, and whether the job asked for the right size.
package verdict

import (
	"fmt"
	"time"
)

// Activity is what the job is doing with the GPU. It is a lifecycle, not a
// yes/no: a job we cannot judge yet sits in Analyzing until it has run long
// enough, or done real work, for a verdict to be honest.
type Activity int

const (
	// Analyzing is the safe default: still warming up, or not enough
	// telemetry to judge. It is the zero value on purpose, so an unset
	// Verdict reads as "not enough data" rather than as an all-clear.
	Analyzing Activity = iota
	Healthy            // real GPU work observed
	Idle               // sustained near-zero GPU work past warmup
	Zombie             // idle, plus evidence the job is not coming back
)

func (a Activity) String() string {
	switch a {
	case Healthy:
		return "healthy"
	case Idle:
		return "idle"
	case Zombie:
		return "zombie"
	default:
		return "analyzing"
	}
}

// Sizing is whether the job asked for the right amount of GPU. It is judged
// only for healthy jobs: an idle job has no meaningful size, and its waste is
// already reported by Activity.
type Sizing int

const (
	// SizingUnknown means not judged - the job is not healthy, or there is
	// no memory telemetry to judge it with.
	SizingUnknown Sizing = iota
	RightSized
	OverProvisioned  // asserted: peak memory never approached the card's size
	UnderProvisioned // hinted only: signs of memory and compute pressure
)

// String is the human form, for the table.
func (s Sizing) String() string {
	switch s {
	case RightSized:
		return "right-sized"
	case OverProvisioned:
		return "over-provisioned"
	case UnderProvisioned:
		return "possibly under-provisioned"
	default:
		return "unknown"
	}
}

// Code is the machine form, for a Prometheus label value. Label values may
// not contain spaces, so String is unusable there.
func (s Sizing) Code() string {
	switch s {
	case RightSized:
		return "right_sized"
	case OverProvisioned:
		return "over_provisioned"
	case UnderProvisioned:
		return "under_provisioned"
	default:
		return "unknown"
	}
}

// Confidence grades how much to trust the verdict. What we measured directly
// is high; what we inferred is lower. Analyzing carries no confidence at all.
type Confidence int

const (
	ConfNone Confidence = iota
	ConfLow
	ConfMedium
	ConfHigh
)

func (c Confidence) String() string {
	switch c {
	case ConfLow:
		return "low"
	case ConfMedium:
		return "medium"
	case ConfHigh:
		return "high"
	default:
		return "n/a"
	}
}

// Enrichment cut-offs. These are constants rather than Thresholds fields on
// purpose: they change how loudly Squire speaks, never what it finds, so they
// are not an operator's dial. Anything that can change a finding belongs in
// Thresholds where it can be overridden and, later, recorded.
const (
	unlitEngineFrac = 0.05 // engine activity below this contradicts a high util reading
	shallowSMFrac   = 0.20 // SM activity below this means busy but shallow
	idleTensorFrac  = 0.05 // tensor activity below this means no tensor work
)

// Metrics is everything the judge needs about one job, already aggregated
// into windows by the caller. Utilization values are percentages (0-100, the
// unit DCGM reports); Frac and Active values are ratios (0-1). Mixing the two
// up is the easiest mistake to make here, which is why the units are spelled
// out on every field.
//
// Every signal carries a Has flag. A signal that was not scraped must never
// arrive as a silent zero - "we did not measure it" and "we measured nothing"
// are different facts and lead to different verdicts.
type Metrics struct {
	Elapsed  time.Duration // job runtime so far
	HasStart bool          // Slurm reported a real start_time

	// GPU utilization drives the activity verdict.
	HasUtil  bool
	AvgUtil  float64 // average over the whole run (%)
	PeakUtil float64 // peak over the idle window (%)

	// Framebuffer memory drives the sizing verdict. The fractions are of the
	// card's total memory, which is derived from used+free rather than
	// assumed from a hardware table.
	HasMem      bool
	PeakMemFrac float64 // peak over the whole run (0-1)
	AvgMemFrac  float64 // average over the whole run (0-1)
	PeakMemMiB  float64 // for display
	CapacityMiB float64 // for display

	// Enrichment signals refine confidence and never change a finding. Each
	// needs DCGM profiling metrics, which plenty of clusters do not scrape,
	// so a verdict that depended on them would mean different things on
	// different clusters for reasons invisible in the output.
	HasGrEngine  bool
	PeakGrEngine float64 // compute engine active, peak over the idle window (0-1)
	HasSM        bool
	PeakSM       float64 // fraction of SMs active, peak over the idle window (0-1)
	HasTensor    bool
	PeakTensor   float64 // tensor pipe active, peak over the idle window (0-1)
	HasPower     bool
	PeakPowerW   float64 // peak power draw over the idle window (watts)
}

// Thresholds are the tunable knobs. An operator overrides them globally for
// the whole instance, never per job - otherwise anyone could tune away an
// inconvenient verdict on their own work. They travel as one struct so the
// whole opinion can later be stamped onto a stored verdict row.
type Thresholds struct {
	GraceCeiling time.Duration // warmup ceiling
	BurstUtilPct float64       // util (%) that counts as real work and ends warmup early
	BurstMemFrac float64       // memory fraction that counts as real work

	IdleWindow   time.Duration // trailing window peak util is measured over
	IdleUtilPct  float64       // peak util (%) below which the GPU is doing nothing
	ZombieAfter  time.Duration // how long idle must persist to become a zombie
	WorkedAvgPct float64       // avg util (%) proving the job did real work earlier

	MemFloorFrac float64 // peak memory below this means over-provisioned
	// UnderUtilPct does double duty: the under-provisioned bar, and the
	// ceiling below which over-provisioned may be asserted at all. One
	// number, because "is this job working the card hard?" is one question.
	UnderUtilPct float64 // avg util at or above this is an under-provisioned candidate
	UnderMemFrac float64 // avg memory at or above this is an under-provisioned candidate
}

// DefaultThresholds is Squire's opinion. Every number leans toward
// under-flagging: a missed zombie costs a little money, a false accusation
// costs the tool its credibility.
func DefaultThresholds() Thresholds {
	return Thresholds{
		GraceCeiling: 15 * time.Minute,
		BurstUtilPct: 15,
		BurstMemFrac: 0.02,
		IdleWindow:   30 * time.Minute,
		IdleUtilPct:  5,
		ZombieAfter:  2 * time.Hour,
		WorkedAvgPct: 10,
		MemFloorFrac: 0.30,
		UnderUtilPct: 90,
		UnderMemFrac: 0.90,
	}
}

// Verdict is the judge's output: one activity, one sizing, a confidence, and
// the evidence behind them, most important reason first.
type Verdict struct {
	Activity   Activity
	Sizing     Sizing
	Confidence Confidence
	Reasons    []string
}

// Summary is the compact one-line form, e.g. "zombie:high / over-provisioned".
func (v Verdict) Summary() string {
	s := v.Activity.String()
	if v.Confidence != ConfNone {
		s += ":" + v.Confidence.String()
	}
	if v.Sizing != SizingUnknown {
		s += " / " + v.Sizing.String()
	}
	return s
}

// Reason returns the single most important line, for a narrow table column.
func (v Verdict) Reason() string {
	if len(v.Reasons) == 0 {
		return ""
	}
	return v.Reasons[0]
}

// Judge applies the rules. It never panics on a missing signal; it downgrades
// the verdict and says why.
func Judge(m Metrics, th Thresholds) Verdict {
	v := Verdict{}

	// Can we judge at all? No telemetry and no start time both mean "not
	// enough data", which is Analyzing. Silence is never an all-clear.
	if !m.HasUtil {
		v.Reasons = append(v.Reasons, "no GPU telemetry observed yet")
		return v
	}
	if !m.HasStart {
		v.Reasons = append(v.Reasons, "no start time from Slurm - cannot age the job")
		return v
	}

	// Warmup. A job's first minutes are data loading, checkpoint restore and
	// kernel compiles, so the GPU legitimately sits near zero. Warmup ends at
	// the ceiling or at the first real burst, whichever comes first.
	//
	// The burst only ever ends warmup EARLY, and only for honest jobs. A
	// zombie never bursts, so it waits out the full ceiling and is then
	// judged - it convicts itself by never doing anything.
	burst := m.PeakUtil >= th.BurstUtilPct && (!m.HasMem || m.PeakMemFrac >= th.BurstMemFrac)
	if m.Elapsed < th.GraceCeiling && !burst {
		v.Reasons = append(v.Reasons, fmt.Sprintf("warming up (%s of %s, no real burst yet)",
			round(m.Elapsed), round(th.GraceCeiling)))
		return v
	}

	// Activity: healthy, idle, or zombie.
	if m.PeakUtil >= th.IdleUtilPct {
		v.Activity = Healthy
		v.Reasons = append(v.Reasons, fmt.Sprintf("GPU util peak %.0f%% over %s",
			m.PeakUtil, round(th.IdleWindow)))
	} else {
		// Idle is the symptom, not the accusation. Promote to zombie only
		// when there is a reason to believe the job is not coming back: it
		// has been idle a long time, or it clearly did work earlier and then
		// went flat, which is what a crash that leaves the allocation behind
		// looks like from the outside.
		longIdle := m.Elapsed >= th.ZombieAfter
		worked := m.AvgUtil >= th.WorkedAvgPct
		switch {
		case longIdle || worked:
			v.Activity = Zombie
			if longIdle {
				v.Reasons = append(v.Reasons, fmt.Sprintf("idle %s (peak GPU %.0f%% over %s)",
					round(m.Elapsed), m.PeakUtil, round(th.IdleWindow)))
			}
			if worked {
				v.Reasons = append(v.Reasons, fmt.Sprintf("did real work earlier (avg %.0f%%) then went idle - likely crashed",
					m.AvgUtil))
			}
		default:
			v.Activity = Idle
			v.Reasons = append(v.Reasons, fmt.Sprintf("no GPU work for %s (peak %.0f%%) - not yet long enough to call abandoned",
				round(m.Elapsed), m.PeakUtil))
		}
	}

	v.Confidence = activityConfidence(&v, m, th)
	v.Sizing = judgeSizing(&v, m, th)
	return v
}

// activityConfidence grades the activity call. Where the deeper signals
// disagree with a busy-looking utilization, it lowers confidence and appends
// the disagreement as a reason rather than quietly overriding the finding.
func activityConfidence(v *Verdict, m Metrics, th Thresholds) Confidence {
	switch v.Activity {
	case Healthy:
		// Utilization is an occupancy flag: one small kernel running the
		// whole sample window reads as 100%. The deeper signals are read
		// broadest-first. If they look fine, or are simply not scraped,
		// trust the reading.
		if m.HasGrEngine && m.PeakGrEngine < unlitEngineFrac {
			// The engine was barely busy, so the utilization reading is
			// uncorroborated. Still healthy - an optional metric does not
			// convict - but said quietly.
			v.Reasons = append(v.Reasons, fmt.Sprintf("but compute engine active only %.0f%% - utilization not corroborated",
				m.PeakGrEngine*100))
			return ConfLow
		}
		if m.HasSM && m.PeakSM < shallowSMFrac {
			v.Reasons = append(v.Reasons, fmt.Sprintf("but SM activity only %.0f%% - busy but shallow (check data loading)",
				m.PeakSM*100))
			return ConfMedium
		}
		if m.HasTensor && m.PeakTensor < idleTensorFrac {
			v.Reasons = append(v.Reasons, fmt.Sprintf("tensor cores near idle (%.0f%%) - util may not mean training throughput",
				m.PeakTensor*100))
			return ConfMedium
		}
		return ConfHigh
	case Zombie:
		// A long-idle zombie is measured. A crash-signature zombie is
		// inferred - strong, not certain - unless a second, independent
		// signal agrees the engine is flat, which is corroboration.
		if m.Elapsed >= th.ZombieAfter {
			return ConfHigh
		}
		if m.HasGrEngine && m.PeakGrEngine < unlitEngineFrac {
			v.Reasons = append(v.Reasons, fmt.Sprintf("compute engine also flat (%.0f%%) - corroborates the idle reading",
				m.PeakGrEngine*100))
			return ConfHigh
		}
		return ConfMedium
	case Idle:
		return ConfMedium
	default:
		return ConfNone
	}
}

// judgeSizing answers "did a WORKING job ask for the wrong amount of GPU?".
// For anything not healthy the question is moot: the waste is the idle
// allocation itself, and Activity already reported it.
func judgeSizing(v *Verdict, m Metrics, th Thresholds) Sizing {
	if v.Activity != Healthy || !m.HasMem {
		return SizingUnknown
	}
	// Over-provisioned is asserted, because "peak never crossed the floor"
	// is a hard fact over the whole run. It is asserted only when the job
	// also was not working the card hard, though: a compute-bound job with a
	// small memory footprint is correctly sized, and calling it
	// over-provisioned is exactly the false accusation to avoid. Low memory
	// alone is not evidence; low memory and unremarkable utilization is.
	if m.PeakMemFrac < th.MemFloorFrac && m.AvgUtil < th.UnderUtilPct {
		v.Reasons = append(v.Reasons, fmt.Sprintf("reserved a full GPU but peak memory only %s (%.0f%%) - never needed a card this large",
			memStr(m.PeakMemMiB, m.CapacityMiB), m.PeakMemFrac*100))
		return OverProvisioned
	}
	// Under-provisioned is hinted, never asserted: a maxed-out card can mean
	// a healthy job or a starving one, and we cannot tell which from outside.
	// The wording carries that uncertainty.
	if m.AvgUtil >= th.UnderUtilPct && m.AvgMemFrac >= th.UnderMemFrac {
		v.Reasons = append(v.Reasons, fmt.Sprintf("pinned near 100%% util with memory ~full (%.0f%%) - possibly too small for this job",
			m.AvgMemFrac*100))
		return UnderProvisioned
	}
	return RightSized
}

// round trims a duration to whole minutes so the output stays readable.
func round(d time.Duration) time.Duration { return d.Round(time.Minute) }

// memStr renders used and total GPU memory in GiB, e.g. "3.0/16.0 GiB".
func memStr(usedMiB, totalMiB float64) string {
	return fmt.Sprintf("%.1f/%.1f GiB", usedMiB/1024, totalMiB/1024)
}
