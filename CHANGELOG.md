# Changelog

## [Unreleased]

Squire ships as a container image and as plain binaries, so there is a version to pin, deploy and report bugs against rather than a repository to clone and build.

### Squire

Reads Slurm, Prometheus and the Kubernetes API and reports, per job, what its GPUs are actually doing.

- **Two verdicts per job, not one score.** *Activity* is a lifecycle — `analyzing`, `healthy`, `idle`, `zombie` — and *Sizing* asks separately whether the card was bigger than the job needed. Each carries a confidence and the evidence behind it in plain English.
- **Telemetry scoped to the devices a job holds**, read from Slurm's `gres_detail` rather than to every GPU on its nodes. On a shared node that difference is the whole verdict. Where Slurm does not publish it, output falls back to node-wide and marks the row rather than pretending.
- **Allocation checks**, which need both what a job asked for and what its devices did:
  - `partially-used-allocation` — holds more GPUs than it has ever lit
  - `gpu-requested-never-touched` — holds GPUs and none has done any work
  - `slow-first-gpu-work` — a long gap before the first device started
  - `blocking-idle-allocation` — the above, while other jobs are waiting for GPUs
- **A `/metrics` endpoint** with per-job utilization, wasted GPU-hours, devices held and lit, verdict states, and cluster queue pressure.
- **A web view on the same port**, showing every running GPU job with both verdicts, the evidence behind them, the findings and the queue. Built from the same cached pass as `/metrics`, so a page left open costs nothing extra. No JavaScript and no external assets, for clusters with no route out, and it follows the browser's light or dark setting.
- **A flag the running mode does not read now says so** instead of being silently dropped.
- **`--watch` for a live table, `--dollar-rate` to cost the waste, `--wide` for the evidence**, and `--act` to record findings as Kubernetes Events on the offending pod.

### squire-lint

Reads one slurmrestd URL. No DCGM, no Prometheus, no kubeconfig — it runs on a login node where the monitoring binary cannot.

- **Seven checks** over what a job asked for and what the cluster can give it: `dependency-doomed`, `no-time-limit`, `time-limit-at-partition-max`, `cpu-only-on-gpu-node`, and three ways to strand a GPU you never requested — `exclusive-over-allocation`, `memory-over-allocation`, `cpu-over-allocation`.
- **Exit codes for scripting**: `0` clean, `1` findings, `2` could not read the job list. A broken token and a clean queue do not look the same.
- **Only live jobs are judged.** Slurm keeps finished jobs in its response for `MinJobAge`, and a finished job cannot be told to set a time limit.

### Running it

- **Container image** for `linux/amd64` and `linux/arm64`, carrying both binaries on a distroless base as a non-root user. Around 35 MB.
- **Kubernetes manifests** in `deploy/` — a ServiceAccount, a namespaced Role granting pod reads and event creation and nothing else, a Deployment and a Service.
- **`--slurm-token-file`**, read on every request. Replacing a rotated Slurm token takes effect on the next scrape without restarting the pod.
- **`--version`** on both binaries, reporting the tag they were built from.
- **Plain binaries and checksums** attached to each release, for running `squire-lint` where a container makes no sense.

### Fixed

- Squire could not use its own Kubernetes credentials inside a pod. It filled in `~/.kube/config` before asking the client library, which only falls back to the in-cluster service account when given no path at all — so it looked for a file that was never going to exist.
- The Slurm token was read once at startup and held for the life of the process. When it expired, every call returned `511` until somebody restarted the pod, and re-minting elsewhere could not help.

### Known limitations

- **Nothing renews the Slurm token.** The Slurm operator's `Token` resource has a `refresh` field, but its controller was not observed reissuing — a ten-minute token went twenty minutes without renewal. Use a long lifetime with an unprivileged account, and replace it deliberately. Squire picks up a replacement without restarting.
- **`--act` can record a finding twice.** The guard preventing a job being stamped twice is held in memory, so a new process — a restarted pod, or a second one-shot run — stamps again for jobs it already reported. Kubernetes expires Events after an hour by default, so the window is bounded, but a pod restarting often will produce noise. The manifests ship the flag commented out.
- **`squire` needs a kubeconfig**, so it is operator-side. There is no user-facing view of the telemetry findings yet; `squire-lint` is what a user runs today.
- **Neither `/metrics` nor the web view is authenticated.** Both name users and jobs, and the metrics endpoint always has, so a login on one and not the other would protect nothing. Keep the port behind `kubectl port-forward` or a NetworkPolicy, and put authentication in front of it before making it reachable any other way.
- On a site running `PrivateData=jobs`, an unprivileged token sees only its owner's jobs and the output narrows accordingly.
