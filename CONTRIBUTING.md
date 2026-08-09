# Contributing to Squire

Thanks for looking. Squire is small on purpose, so the fastest way to get a
change merged is to know what it is trying to be.

## What Squire is

Squire states one thing Slurm structurally cannot see: that a running
allocation is doing no real work. It joins three sources — the Slurm REST API,
the Kubernetes API, and DCGM telemetry from Prometheus — and produces a verdict
per job with the evidence behind it.

It is **read-only**. It does not schedule, preempt, reorder, or cancel
anything. The only thing it writes is a Kubernetes Event, and only when asked
with `--act`. A pull request that makes Squire act on the cluster is out of
scope, however useful the action.

## Sign your commits

Squire uses the [Developer Certificate of Origin](https://developercertificate.org/),
not a CLA. Adding `-s` to your commit appends a `Signed-off-by` line, which is
you asserting you have the right to submit the code:

```
git commit -s -m "fix(report): ..."
```

If you forget on the last commit:

```
git commit --amend -s --no-edit
```

## Before you open a pull request

Everything must pass:

```
gofmt -l . && go vet ./... && go test -race ./...
```

`gofmt -l` exits 0 even when it lists files, so read its output rather than
trusting the exit code — empty output is the pass condition. The `-race` flag
matters: some tests are meaningless without it.

**Every decision rule lives in `internal/verdict`, which imports nothing but
`fmt` and `time`.** No Prometheus, no Slurm, no Kubernetes. That constraint is
what makes every rule testable on a laptop, and it is worth protecting. If a
change needs cluster access to be judged, it probably belongs in
`internal/report` instead.

Commits follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat`, `fix`, `docs`, `test`, `refactor`, `chore`), with a subject under
about 70 characters and a body wherever there was a judgement call. Explain
why, not what — the diff already says what.

## Changing a threshold

This is the one thing worth setting a standard for, because Squire's entire
product is a set of judgement calls about when to accuse someone of wasting a
GPU.

**Every default leans toward under-flagging.** A missed zombie costs a little
money. A false accusation costs the tool its credibility, and credibility is
the only reason anyone would leave it running. If your change makes Squire flag
more jobs, the bar is higher than "it caught something on my cluster."

A threshold change needs:

- **What you measured, over how long.** A number that looks wrong at two
  minutes is usually a young measurement. Squire has already demonstrated this
  on itself: a job read `over-provisioned` at two minutes and `right-sized` at
  ten, with no configuration change, because its lifetime average was still
  climbing.
- **What the job actually was.** "Training run, 8×A100, fp16" is evidence.
  "A job on my cluster" is not.
- **Which verdicts change and which do not.** If the change moves a job from
  `idle` to `zombie`, say what convinced you the job was not coming back.

Adding a *new* signal is usually easier to justify than moving an existing
number, and it is the safer shape of change — see below.

## Adding a signal

Signals fall into two classes and the split is load-bearing:

- **Signals that produce verdicts.** GPU utilization and framebuffer memory.
  These must be available on any cluster running dcgm-exporter's default
  counter set, or Squire means different things in different places for
  reasons invisible in its output.
- **Signals that only adjust confidence.** The DCGM profiling fields. These may
  lower or raise confidence and add a reason line. **They must never change a
  finding.** There is a test asserting exactly that, and it should stay.

Every signal carries a `Has*` flag through the judge. A metric that was not
scraped must never arrive as a silent zero — "we did not measure it" and "we
measured nothing" lead to opposite verdicts.

## Reporting a bug

Include the Squire version or commit, your Slurm and Kubernetes versions,
whether `gres_detail` is available on your cluster, and the output of:

```
go run ./cmd/squire --wide
```

The `--wide` output carries the evidence behind each verdict, which is usually
enough to tell a wrong rule from missing telemetry. If the verdict looks wrong,
say what you believe the job was actually doing — that is the part nobody can
reconstruct from logs.

## What is likely to be declined

- Anything that makes Squire write to the cluster beyond Events.
- Per-job threshold overrides. Thresholds are global to the instance on
  purpose, so nobody can tune away an inconvenient verdict on their own job.
- Keying behaviour on a name a site can choose for itself — a partition called
  `gpu`, a node prefix, a queue name. "Is this a GPU job" is `gres/gpu` in the
  allocated TRES, and the Slurm-node-to-pod mapping is read from an operator's
  own label.
- A verdict that depends on an optional metric.

