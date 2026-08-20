# Changelog

## v0.1.0 — 2026-08-20

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

### squire-me

Answers "what are my jobs doing" for the person who submitted them, from a login node, with no credentials at all — no Slurm token, no kubeconfig, no Prometheus. It asks a running `squire --serve` for the pass it has already built.

- **Your own jobs, decided by your user id**, or somebody else's with `--user` or `--uid`. Nothing verifies whose jobs you may see — the same as the web view — so this is a filter rather than a permission.
- **Findings come with it, in two blocks**: allocation findings about the jobs in the table, and configuration findings about what jobs asked for. The second kind covers jobs the table cannot show, so an empty table above a `CONFIGURATION` block is a normal answer.
- **`--json` prints the server's document untouched**, for `jq`. Values that were never measured are `null`, never `0`.
- **Exit codes are `squire-lint`'s**: `0` clean, `1` findings, `2` could not ask. It refuses a document whose schema it does not read, before printing anything.
- **Point it with `SQUIRE_URL` or `--url`.** It does nothing on its own: without a reachable `squire`, the first run is a connection error.

### One pass, two engines, three surfaces

- **`/snapshot.json`** publishes the pass as a typed document with a `schema` number, served from the same cache as `/metrics` and the web view, so the three cannot disagree. `?uid=` narrows it to one owner, server-side.
- **The configuration checks now run inside `squire` too**, not only in `squire-lint`. They cover every job Slurm knows about — including jobs holding no GPU and jobs that have not started — so a finding can name a job with no row in the verdict table. The two kinds are shown apart, on the table and on the page.
- **An eighth check, `unsatisfiable-request`**: the job asks each node for more GPUs than any node in its partition has, so no amount of waiting will start it. Slurm reports it as waiting for resources, which is the same word a job behind a busy queue gets.
- **`--rule` and `--job` on both binaries**, and the same two as query parameters on the web view and the document. They narrow findings, never the job table, and an unmatched rule is an empty answer rather than an error.
- **Unmeasured telemetry is no longer published as zero.** A job whose telemetry has not arrived reads as a dash on the table and `null` in the document, and its derived metrics are not emitted at all — a measured zero and an unread signal are opposite facts.

### Running it

- **Container image** for `linux/amd64` and `linux/arm64`, carrying both binaries on a distroless base as a non-root user. Around 35 MB.
- **Kubernetes manifests** in `deploy/` — a ServiceAccount, a namespaced Role granting pod reads and event creation and nothing else, a Deployment and a Service.
- **`--slurm-token-file`**, read on every request. Replacing a rotated Slurm token takes effect on the next scrape without restarting the pod.
- **`--version`** on both binaries, reporting the tag they were built from.
- **Plain binaries and checksums** attached to each release, for running `squire-lint` and `squire-me` where a container makes no sense. On a login node take `squire-lint-linux-amd64` or `squire-me-linux-amd64`, and check it against `SHA256SUMS` before running it:

```bash
  sha256sum -c SHA256SUMS --ignore-missing
```
- **`squire_build_info` says when the build recorded no source revision** — `unrecorded` — instead of claiming a clean tree it never looked at. A container build has no repository to read.

### Fixed

- Squire could not use its own Kubernetes credentials inside a pod. It filled in `~/.kube/config` before asking the client library, which only falls back to the in-cluster service account when given no path at all — so it looked for a file that was never going to exist.
- The Slurm token was read once at startup and held for the life of the process. When it expired, every call returned `511` until somebody restarted the pod, and re-minting elsewhere could not help.

### Known limitations

- **Nothing renews the Slurm token.** The Slurm operator's `Token` resource has a `refresh` field, but its controller was not observed reissuing — a ten-minute token went twenty minutes without renewal. Use a long lifetime with an unprivileged account, and replace it deliberately. Squire picks up a replacement without restarting.
- **`--act` can record a finding twice.** The guard preventing a job being stamped twice is held in memory, so a new process — a restarted pod, or a second one-shot run — stamps again for jobs it already reported. Kubernetes expires Events after an hour by default, so the window is bounded, but a pod restarting often will produce noise. The manifests ship the flag commented out.
- **`squire` needs a kubeconfig**, so it is operator-side. Users reach the same results through `squire-me`, which needs none.
- **None of `/metrics`, the web view or `/snapshot.json` is authenticated.** Both name users and jobs, and the metrics endpoint always has, so a login on one and not the other would protect nothing. Keep the port behind `kubectl port-forward` or a NetworkPolicy, and put authentication in front of it before making it reachable any other way.
- On a site running `PrivateData=jobs`, an unprivileged token sees only its owner's jobs and the output narrows accordingly. If privacy is important, restrict access to the Squire web service.
