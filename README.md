# squire

Finds GPUs that Slurm has allocated but nobody is using. Joins slurmrestd, DCGM and Kubernetes pod identity into per-job efficiency metrics and Events.

Two binaries. **`squire`** measures what GPUs are doing and needs the telemetry stack. **`squire-lint`** checks what jobs asked for and needs one slurmrestd URL — no DCGM, no Prometheus, no kubeconfig, so it runs on a login node where the other one cannot.

---

## What Squire reports

Squire answers three questions, and keeping them apart is what keeps the answers honest.

| Report | Reads | Answers |
|---|---|---|
| **Verdicts** | DCGM telemetry, per device | What is this GPU doing? |
| **Job checks** | slurmrestd alone | What did this job ask for, and can the cluster give it? |
| **Allocation checks** | both | Is the job using everything it was given? |

`squire-lint` runs the job checks. `squire` produces the verdicts and the allocation checks.

**`squire` is operator-side.** It needs a kubeconfig, because the Slurm-node-to-pod mapping is what scopes GPU telemetry to a job's own devices. A user on a login node has no kubeconfig and should not need one, which is why `squire-lint` is a separate binary that reads only slurmrestd.

Both ship in one container image and as plain binaries. **[Running it in a cluster](docs/deployment.md)** covers the manifests, the Slurm token, and what Squire is granted.

A verdict describes a device. A check produces a *finding* — the word the output uses — which names somebody's job and is read by their colleagues. That is why the bar for producing one is higher than for reporting a number.

---

## The verdict model

Squire produces **two independent verdicts per job**, because "is this job wasting a GPU" turns out to be two separate questions.

**Activity — what the GPU is doing.** A lifecycle, not a flag:

| State | Meaning |
|---|---|
| `analyzing` | Inside the warmup window, or not enough telemetry to judge. **The safe default** — Squire says "not enough data" rather than guessing. |
| `healthy` | Real GPU work observed. |
| `idle` | Sustained near-zero work past warmup. A measurement, not an accusation. |
| `zombie` | Idle **plus** evidence it isn't coming back (long duration, or the crash signature). |

**Sizing — whether the card is bigger than the job needed.** This axis is about one device's capacity, not how many devices the job holds: it compares peak memory against the card's total and asks whether the job worked that card hard. Judged only for `healthy` jobs, since an idle job's waste is already reported by Activity:

| State | Meaning |
|---|---|
| `over-provisioned` | **Asserted.** Peak memory over the whole run never approached the card's size, *and* the job was not working the card hard. Both, because a compute-bound job with a small footprint is correctly sized. |
| `right-sized` | No sizing flag fired. |
| `possibly under-provisioned` | **Hinted only.** A maxed-out card can mean healthy-and-perfect or starving-for-more, so Squire never asserts this. |

Every verdict carries a **confidence** (`high` / `medium` / `low`) and the evidence behind it in plain English.

**Idle → Zombie promotion.** Idle becomes Zombie when either is true:

- **Duration** — idle has persisted past `--zombie-after` (default 2h).
- **The crash signature** — the job's lifetime average proves it did real work, but its recent peak is flat. e.g. The training died; the allocation lived on.

**Why "no burst ever" is not a detection gap.** Warmup ends at a fixed ceiling *or* the first real burst, whichever comes first. The burst only ever ends warmup *early*, for honest jobs. A true zombie never bursts, so warmup ends at the ceiling and the idle window then counts against it. The zombie convicts itself by never bursting.

---

## Job checks: what a job asked for

`squire-lint` reads three slurmrestd endpoints — jobs, nodes, partitions — and writes nothing anywhere. It needs no telemetry stack at all, which is the point: it works on a cluster that has installed none of it.

| Rule | Severity | Fires when |
|---|---|---|
| `dependency-doomed` | warn | Slurm's own `state_reason` is `DependencyNeverSatisfied` |
| `no-time-limit` | warn | No wall-clock limit, or an unlimited one — the scheduler cannot backfill around either |
| `time-limit-at-partition-max` | note | The limit is exactly the partition ceiling, usually the default rather than an estimate |
| `cpu-only-on-gpu-node` | warn | A job holding no GPUs occupies a node that has them |
| `exclusive-over-allocation` | warn | `--exclusive` with a GRES request smaller than the node |
| `memory-over-allocation` | warn | The job holds all of a node's memory while requesting only some of its GPUs |
| `cpu-over-allocation` | warn | The job holds all of a node's cores while requesting only some of its GPUs |

`warn` is worth fixing. `note` is worth knowing. Two levels rather than five, because more invites arguing about the grade instead of the finding.

**Three rules cover the same damage through different resources.** Exclusivity, memory and cores each leave GPUs allocated and unschedulable, and each is invisible in every aggregate because the node reads as fully allocated. They are separate rules because the remedy differs: drop or narrow `--exclusive`, pass `--mem`, pass `-c`. One combined rule would have to name all three fixes and would be right about one.

**Exclusivity has three forms** — `--exclusive`, `--exclusive=user`, `--exclusive=mcs` — and all three strand the same GPUs. The finding says who is shut out, because that decides which remedy applies.

**Which jobs are checked.** Pending, running, suspended and configuring. Slurm keeps finished jobs in its response for `MinJobAge`, and a finished job cannot be told to set a time limit or accused of holding a node it already gave back. The summary line reports how many were skipped.

**Exit codes**, because anything scripting this depends on them:

| Code | Meaning |
|---|---|
| 0 | Checked, no findings |
| 1 | Findings |
| 2 | The job list could not be read |

A run that reads jobs but not nodes still checks what it can and exits 0 or 1, saying so in the output. `2` means the check did not happen.

**Scope.** It checks submitted jobs, running ones included. It is not a pre-submission hook.

---

## Allocation checks: when a job holds more than it uses

These need both halves. *"Requested eight GPUs"* is a configuration fact that says nothing about waste. *"Two devices were busy"* is a measurement that says nothing about intent. Together they name a specific job holding specific hardware it never used.

| Rule | Severity | Fires when |
|---|---|---|
| `partially-used-allocation` | warn | The job holds more GPUs than it has ever lit |
| `gpu-requested-never-touched` | warn | The job holds GPUs and none has ever done work |
| `slow-first-gpu-work` | note | A long gap between the job starting and its first device working |
| `blocking-idle-allocation` | warn | The job holds devices it has not lit while other jobs wait for GPUs |

Measured on a 4-GPU node, two jobs each holding two devices:

```
JOBID  NAME  USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM       WASTED-GPU-H  ACTIVITY        SIZING            LIT  FIRST-WORK
192    wrap  uid:50000  2     26m0s    49    100    0.6/15G (4%)  0.4           healthy:medium  over-provisioned  1    58s
193    wrap  uid:50000  2     26m0s    37    100    0.6/15G (4%)  0.5           healthy:medium  over-provisioned  2    16m51s

JOBID  NAME  SEVERITY  RULE                       FINDING
192    wrap  warn      partially-used-allocation  holds 2 GPUs but only 1 has done any work - the other 1 is allocated and idle
193    wrap  note      slow-first-gpu-work        17m passed before any GPU did work - startup, staging or initialization held 2 GPUs idle
```

Both jobs read `healthy:medium` and `over-provisioned`. Looking only at utilization, they are two ordinary jobs. The findings underneath say one has held an idle GPU for twenty-six minutes and the other held two for seventeen — and no aggregate anywhere would show it, because the node reads as fully allocated the whole time.

**`LIT` counts devices that ever crossed the burst threshold, and answers a question an average cannot.** Averaged across devices, four busy cards and eight half-busy ones read the same.

**`FIRST-WORK` separates startup idleness from waste idleness.** Container pulls, dataset staging and kernel compilation are not the same as an abandoned allocation, and nothing else Squire measures tells them apart.

### When idle stops being free

The first three are true whether the cluster is empty or full. Four devices held and one lit costs nothing on a quiet Saturday and costs three teams their afternoon on a Tuesday, and nothing above can tell those apart.

`blocking-idle-allocation` is that difference. Measured on a single-GPU node with one job holding it and another waiting:

```
JOBID  NAME  USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM       WASTED-GPU-H  ACTIVITY     SIZING  LIT  FIRST-WORK
195    wrap  uid:50000  1     3m0s     0     0      0.0/15G (0%)  0.1           idle:medium  -       0    -

JOBID  NAME  SEVERITY  RULE                         FINDING
195    wrap  warn      blocking-idle-allocation     its 1 GPU has done no work, while 1 job waits for 1 GPU
195    wrap  warn      gpu-requested-never-touched  holds 1 GPU and none has done any work in 3m0s - the job may not be able to see them at all
```

**It reports two facts measured at the same instant, not a causal chain.** The sentence looks like an accusation that job 195 is blocking a specific waiting job. It is not. Deciding which pending job would have landed where depends on partitions, features, memory, topology and priority — that is Slurm's decision, and Squire does not make it. What it can say is that idle devices and unmet demand exist right now, which is the fact somebody needs in order to go and look.

**It clears itself.** Cancel the waiting job and the finding disappears while `gpu-requested-never-touched` stays. The allocation is exactly as idle; it is simply not costing anybody anything. Expect that behaviour rather than reporting it as a flapping alert.

**Only jobs waiting on `Resources` count.** A job held by priority, a dependency, an administrator or a licence would not start if every GPU on the cluster went free this second. Counting those would report pressure that is not there.

**On a site that sets `PrivateData=jobs`**, a token sees only its owner's jobs, so the pending count falls to whatever that token can see. The finding gets quieter, never wrong.

**When these stay silent**, which matters as much as when they fire:

- **Inside the grace period.** A job that has just started has not had the chance to waste anything.
- **Without per-device telemetry.** If the numbers cover every GPU on the job's nodes rather than the ones it holds, a neighbour's work could exonerate it or a neighbour's idleness could condemn it. Either way the finding would name the wrong person.
- **Without the measurement.** An unread device count is never treated as zero. In `--wide` a dash means "not measured"; `0` means "measured, and nothing was lit".
- **With nobody waiting**, for `blocking-idle-allocation` alone. The other three do not care what the queue is doing.

---

## Per-job attribution

Telemetry is scoped to the **exact GPU devices a job holds**, read from Slurm's `gres_detail`, not to every GPU on its nodes. On a shared node that difference is the whole verdict: an idle job reading its neighbour's utilization was measured at 97% against a real Prometheus when it was doing nothing at all.

Where Slurm does not publish `gres_detail`, Squire falls back to node-wide scoping and **marks the row with `*`** plus a footnote. It never quietly presents node-wide numbers as if they were the job's own.

Measured on a shared 4-GPU node: two jobs, one pod, one holding GPUs 0-1 and the other GPUs 2-3. DCGM read 0% on the first pair and 100% on the second at the same moment, and Squire reported each job only its own. Scoped by pod alone, both jobs would have read 100%.

Slurm counts device minor numbers and DCGM counts NVML indices, and nothing guarantees those agree. Joined by UUID on that node they did, card for card.

---

## The signals Squire reads, and why

**DCGM** is NVIDIA's Data Center GPU Manager — the telemetry stack that publishes per-GPU readings. Squire reads them from Prometheus via *dcgm-exporter*; it never touches a GPU directly.

| Signal | DCGM field | Unit | Why Squire reads it |
|---|---|---|---|
| GPU utilization | `DCGM_FI_DEV_GPU_UTIL` | percent, 0–100 | The number everyone watches — and a **time-based occupancy flag, not a work measurement**. One tiny kernel on one of a hundred-plus compute units reads as 100%. |
| Framebuffer used | `DCGM_FI_DEV_FB_USED` | **absolute MiB** | "Framebuffer" is NVIDIA's term for the memory on the card. This is the sizing signal — but an absolute number is meaningless without the card's size. |
| Framebuffer free | `DCGM_FI_DEV_FB_FREE` | **absolute MiB** | Added to `FB_USED` to derive the card's total memory. This is how Squire learns it is looking at a 15 GiB card **without being told what hardware it runs on** — which is why the over-provisioned verdict is phrased as an observation, not a hardware recommendation. |
| Engine activity | `DCGM_FI_PROF_GR_ENGINE_ACTIVE` | **ratio, 0–1** | Was the compute engine busy at all. The broad signal, and a more precise answer to the occupancy question than utilization. **Optional.** |
| SM activity | `DCGM_FI_PROF_SM_ACTIVE` | **ratio, 0–1** | SM = Streaming Multiprocessor, one of the GPU's independent compute engines. How *much* of the GPU was busy. The deep signal. **Optional**, and less commonly scraped than the engine one. |
| Tensor-core activity | `DCGM_FI_PROF_PIPE_TENSOR_ACTIVE` | **ratio, 0–1** | The units that actually cost money on a training job. High utilization with idle tensor cores means the job is busy but not training. **Optional.** |
| Power draw | `DCGM_FI_DEV_POWER_USAGE` | **watts** | Informational. Low power with high utilization is suspicious. **Optional.** |

**Where each signal is allowed to matter.** Utilization and framebuffer memory produce verdicts. The profiling signals only ever **adjust confidence** and add a reason line — they catch the "busy but shallow" case (utilization says 100%, SM activity says 12%, so the GPU is probably waiting on the data loader) without being load-bearing. That split is deliberate: a verdict that silently depended on an optional metric would behave differently on two clusters for reasons nobody could see.

**Missing signals degrade, never crash.** Every metric carries a `Has*` flag through the judge. An absent signal costs confidence or drops the Sizing axis to `unknown`; it never becomes an implicit zero, and it never produces a false all-clear.

**A broken sensor is not an idle fleet.** If `GR_ENGINE_ACTIVE` reads flat on every GPU in the cluster while utilization is high on any of them, Squire treats the instrument as faulted, drops the signal for that cycle, and says so. A genuine-looking zero is more dangerous than a missing metric, because it reads as corroboration.

**Never judge on a snapshot.** Every number is an average or a peak over a window — lifetime average for "did this ever work", trailing-window **peak** for "is it working now". Peak, not average, on the window: one honest spike clears a job.

---

## Thresholds

Defaults are the product; configuration is an escape hatch. Every default leans toward **under-flagging** — a missed zombie costs a little money, a false accusation costs the tool its credibility.

| Flag | Default | Governs |
|---|---|---|
| `--grace` | 15m | Warmup ceiling: covers data loading, checkpoint restore, kernel compile. Also the age below which no allocation check will report anything. |
| `--burst-util` | 15 | Utilization (%) counting as real work, ending warmup early. Also the bar a device must cross to count as lit. |
| `--idle-window` | 30m | Trailing window that peak utilization is measured over. |
| `--idle-util` | 5 | Peak utilization (%) below which the GPU is doing nothing. |
| `--zombie-after` | 2h | How long idle must persist before it is a zombie. |
| `--worked-util` | 10 | Lifetime average (%) proving the job did real work earlier. |
| `--mem-floor` | 0.30 | Peak memory fraction below which a job is over-provisioned. |

These are **global to the Squire instance**. There are deliberately no per-job knobs — nobody gets to tune away an inconvenient verdict on their own job.

---

## Setup a dev environment
```
source ./env.sh
./scripts/setup.sh
./scripts/dev-tunnels.sh && source .squire-env
(setup port forwarding for slurm rest API, prometheus and grafana, mint a new JWT Token)
```

## Using Squire

### Submit GPU workload
#### submit a real GPU job
e.g. sbatch train_gpt.sbatch
#### submit a zombie GPU job
e.g. sbatch --gres=gpu:1 --job-name=zombie --wrap="sleep 3600"
#### check queue and verify they are running
squeue -o '%.8i %.10j %.8T %.10M %.10R %b'

### Running Squire
#### Basic Usage
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM        WASTED-GPU-H  ACTIVITY        SIZING
129    tinygpt  uid:50000  1     1m0s     100   100    2.9/15G (20%)  0.0           healthy:medium  right-sized
130    zombie   uid:50000  1     1m0s     0     0      0.0/15G (0%)   0.0           analyzing       -
lmsilva@PANDAMONIUM:~/squire$
```

#### Show the evidence

`--wide` adds the reasoning behind each verdict, plus the two measurements the allocation checks rest on.

```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --wide
JOBID  NAME  USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM       WASTED-GPU-H  ACTIVITY        SIZING            LIT  FIRST-WORK  WHY
192    wrap  uid:50000  2     26m0s    49    100    0.6/15G (4%)  0.4           healthy:medium  over-provisioned  1    58s         GPU util peak 100% over 26m0s; tensor cores near idle (0%) - util may not mean training throughput; reserved a full GPU but peak memory only 0.6/14.7 GiB (4%) - never needed a card this large
193    wrap  uid:50000  2     26m0s    37    100    0.6/15G (4%)  0.5           healthy:medium  over-provisioned  2    16m51s      GPU util peak 100% over 26m0s; tensor cores near idle (0%) - util may not mean training throughput; reserved a full GPU but peak memory only 0.6/14.7 GiB (4%) - never needed a card this large

JOBID  NAME  SEVERITY  RULE                       FINDING
192    wrap  warn      partially-used-allocation  holds 2 GPUs but only 1 has done any work - the other 1 is allocated and idle
193    wrap  note      slow-first-gpu-work        17m passed before any GPU did work - startup, staging or initialization held 2 GPUs idle
lmsilva@PANDAMONIUM:~/squire$
```

Nothing prints below the table when there are no findings.

#### Serve Prometheus metrics endpoint

Do note anyone who can reach this port gets the metrics, and they include usernames. Bind it to localhost or a cluster-internal Service.
Responses are cached for `--serve-cache` (30s by default). Keep it under your Prometheus scrape interval, or you'll scrape the same numbers twice. `--serve-cache 0` turns it off and rebuilds on every scrape.

```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --serve :9101 &
serving /metrics on :9101
lmsilva@PANDAMONIUM:~/squire$ curl -sS localhost:9101/metrics | grep -E "squire_job_gpus_(lit|held)"
# HELP squire_job_gpus_lit GPU devices held by the job that have done work at some point in the run.
# TYPE squire_job_gpus_lit gauge
squire_job_gpus_lit{job_id="192",user="uid:50000",partition="all"} 1
squire_job_gpus_lit{job_id="193",user="uid:50000",partition="all"} 2
# HELP squire_job_gpus_held GPU devices allocated to the job.
# TYPE squire_job_gpus_held gauge
squire_job_gpus_held{job_id="192",user="uid:50000",partition="all"} 2
squire_job_gpus_held{job_id="193",user="uid:50000",partition="all"} 2
lmsilva@PANDAMONIUM:~/squire$
```

#### Act on it by stamping the POD!
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --watch 30s --dollar-rate 0.53 --grace 1m --zombie-after 1m --act
event emitted on slurm-worker-gpu-1 for job 130

=== 10:27:55 ===
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM        WASTED-GPU-H  ACTIVITY        SIZING
129    tinygpt  uid:50000  1     4m0s     99    100    2.9/15G (20%)  0.0 ($0.00)   healthy:medium  right-sized
130    zombie   uid:50000  1     4m0s     0     0      0.0/15G (0%)   0.1 ($0.04)   zombie:high     -
^C
stopping
lmsilva@PANDAMONIUM:~/squire$ ZNODE=$(kubectl -n slurm exec slurm-controller-0 -c slurmctld -- squeue -h -n zombie -o %N)
lmsilva@PANDAMONIUM:~/squire$ ZPOD=$(kubectl -n slurm get pods -l nodeset.slinky.slurm.net/pod-hostname=$ZNODE -o jsonpath='{.items[0].metadata.name}')
lmsilva@PANDAMONIUM:~/squire$ kubectl -n slurm describe pod "$ZPOD" | tail -n 6
Events:
  Type     Reason             Age    From        Message
  ----     ------             ----   ----        -------
  Warning  GPUAllocationIdle  2m23s  squire  slurm job 130 (user uid:50000) holds GPUs with no activity: confidence high: idle 4m0s (peak GPU 0% over 30m0s)
lmsilva@PANDAMONIUM:~/squire$
```

## Using squire-lint

The check reads slurmrestd and nothing else. No DCGM, no Prometheus, no kubeconfig — so it runs on a login node, where the monitoring binary cannot.

#### A clean queue
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire-lint
checked 0 jobs, no findings
```

#### Findings
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire-lint
JOBID  NAME  SEVERITY  RULE                         FINDING
178    wrap  warn      no-time-limit                no time limit set - the scheduler cannot backfill around a job with no end
179    wrap  note      time-limit-at-partition-max  time limit is exactly the partition maximum (24h0m0s), which is usually the default rather than an estimate - a tighter limit backfills sooner
180    wrap  warn      cpu-only-on-gpu-node         requests no GPUs but holds gpu-0, which has 1 - those GPUs are only usable by another job if enough of the node is left free
182    wrap  warn      dependency-doomed            pending on a dependency Slurm says can never be satisfied - it will queue forever. kill_invalid_depend in slurm.conf removes these automatically

4 findings across 4 of 5 jobs (1 already finished)
```

`4 of 5 jobs` is the count actually judged. The fifth had already finished and was skipped.

#### The fast way, if Slurm runs in Kubernetes

Nothing to install and nothing to deploy — one throwaway pod from the published image, using your own Slurm identity.

**Find slurmrestd.** Search by port rather than by name: 6820 is slurmrestd's default, and the Service is called whatever your install called it.

```
lmsilva@PANDAMONIUM:~/squire$ kubectl get svc -A | awk 'NR==1 || /6820/'
NAMESPACE   NAME            TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)    AGE
slurm       slurm-restapi   ClusterIP   10.100.186.89    <none>        6820/TCP   35d
```

**Build the URL from that row** — `http://<NAME>.<NAMESPACE>.svc:6820`. The rest of this section uses a variable, so substitute your own values once here and the commands below are copy-paste:

```
lmsilva@PANDAMONIUM:~/squire$ RESTAPI=http://slurm-restapi.slurm.svc:6820
lmsilva@PANDAMONIUM:~/squire$ NS=slurm
```

The fully qualified `.svc` form resolves from any namespace, so it does not matter where the pod lands.

**Mint a token for yourself** from the controller pod:

```
lmsilva@PANDAMONIUM:~/squire$ TOKEN=$(kubectl -n $NS exec slurm-controller-0 -c slurmctld -- scontrol token username=$USER lifespan=600 | cut -d= -f2)
```

**Run the check:**

```
lmsilva@PANDAMONIUM:~/squire$ kubectl -n $NS run squire-lint --rm -i --restart=Never \
  --image=ghcr.io/lmsilva/squire:v0.1.0 \
  --env="SLURM_JWT=$TOKEN" \
  --command -- /usr/local/bin/squire-lint --slurm-url $RESTAPI
```

`--rm` removes the pod when it exits, and the exit code comes back to your shell, so this works in a script. `--command` is needed because the image's entrypoint is `squire`, not `squire-lint`.

Nothing here is specific to any Slurm operator — a reachable slurmrestd and a token is the whole requirement. For a recurring check, the same `command` in a `CronJob` gives you a nightly report.

#### Options
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire-lint -h
Usage of squire-lint:
  -slurm-api string
        slurmrestd API version (default "v0.0.44")
  -slurm-url string
        slurmrestd base URL (default "http://localhost:6820")
```

## Squire Flags

| Flag | Default | What it does |
|---|---|---|
| `--slurm-url` | `http://localhost:6820` | slurmrestd base URL |
| `--slurm-api` | `v0.0.44` | slurmrestd API version |
| `--slurm-token-file` | unset | read the Slurm token from this file on every request instead of from `SLURM_JWT` |
| `--prom-url` | `http://localhost:9090` | Prometheus base URL |
| `--kubeconfig` | `$KUBECONFIG`, else `~/.kube/config` | kubeconfig path |
| `--namespace` | `slurm` | namespace holding the Slurm worker pods |
| `--pod-hostname-label` | `nodeset.slinky.slurm.net/pod-hostname` | pod label carrying the Slurm node name |
| `--pod-label` | `exported_pod` | DCGM metric label carrying the pod name |
| `--watch` | `0` | refresh interval for top mode (0 = print once) |
| `--wide` | `false` | show the evidence behind each verdict, plus lit devices and time to first work |
| `--serve` | off | expose `/metrics` on this address instead of printing a table |
| `--serve-cache` | `30s` | how long a build is reused before `/metrics` rebuilds (0 disables) |
| `--act` | `false` | emit Kubernetes Events for zombie findings |
| `--dollar-rate` | `0` | $/GPU-hour, for costing the waste column |
| `--grace` | `15m` | warmup ceiling before a job can be judged |
| `--burst-util` | `15` | GPU util % counting as real work, ending warmup early |
| `--idle-window` | `30m` | trailing window peak utilization is measured over |
| `--idle-util` | `5` | peak GPU util % below which the job is idle |
| `--zombie-after` | `2h` | how long idle must persist before it is a zombie |
| `--worked-util` | `10` | avg GPU util % proving the job did real work earlier |
| `--mem-floor` | `0.30` | peak memory fraction below which a job is over-provisioned |

Any argument that is not a flag exits 2 rather than being ignored. Go's flag parsing stops at the first non-flag argument, so a stray word would otherwise silently discard every flag after it.

`--zombie-threshold` and `--zombie-window` were removed. They were named after the one verdict they produced; the rules they governed are now `--idle-util` and `--idle-window`. Passing a removed flag exits 2 with `flag provided but not defined`.

**`--slurm-token-file` exists because a token in an environment variable cannot be replaced.** A process's environment cannot be changed from outside, so a token read at startup is fixed until the process restarts — and when it expires every call returns `511`. A file is re-read on every request, so replacing it is enough and nothing restarts. See [deployment](docs/deployment.md) for how that works in a cluster.

Environment variables: `SLURM_JWT` for the slurmrestd token, and `SQUIRE_SLURM_URL`, `SQUIRE_SLURM_API`, `SQUIRE_PROM_URL`, `SQUIRE_NAMESPACE`, `SQUIRE_POD_HOSTNAME_LABEL`, `SQUIRE_POD_LABEL` as defaults for the flags above.

In `--serve` mode, Squire honours the scrape timeout Prometheus sends and finishes just inside it, so a slow cluster gets an error you can read instead of a dropped connection.

### squire-lint Flags

`squire-lint` takes only three flags — `-slurm-url`, `-slurm-api` and `-slurm-token-file` — and reads `SLURM_JWT` from the environment, never a flag, so the token stays out of `ps` output. It has none of `squire`'s Prometheus, Kubernetes, threshold or serve settings, because it reads none of those things.

Nothing reachable from `squire-lint` imports Kubernetes or Prometheus, so neither is linked into it. That is the difference between a 9.5 MB binary you can hand to a user and a 37 MB one that expects cluster credentials.

## Versions and compatibility

`squire --version` reports what a binary was built as. Releases are tagged `vX.Y.Z`; the image is published for `linux/amd64` and `linux/arm64` on the same tag, alongside plain binaries and checksums.

```
lmsilva@PANDAMONIUM:~/squire$ docker run --rm ghcr.io/lmsilva/squire:dev --version
squire dev go1.26.5 linux/amd64
```

A build that was not stamped says `dev`, which is the truthful answer rather than a version nobody released.

**What a version promises.** Squire is pre-1.0, so the surface is still moving — but not arbitrarily. These are covered by the version number:

- Flag names and their default values
- Exit codes
- Metric names and labels
- Rule identifiers, like `partially-used-allocation`
- Verdict state names, like `zombie` and `over-provisioned`
- The Kubernetes Event reason, `GPUAllocationIdle`

**Deliberately not covered: the wording of findings, and table layout.** Those get better with use, and freezing them would help nobody. Script against rule identifiers and metric names, never against message text.

**Pin a version in anything you deploy.** There is no `latest` tag before 1.0: a moving tag cannot be rolled back to a known state, and two pods started a week apart could be running different code.

## Exported metrics

| Series | Type | Notes |
|---|---|---|
| `squire_job_gpu_utilization_percent` | gauge | Lifetime average utilization per job. |
| `squire_job_gpu_hours_wasted` | gauge | Allocated-but-unused GPU-hours. |
| `squire_job_activity` | gauge | One series per state, `1` on the state held. |
| `squire_job_sizing` | gauge | One series per state, `1` on the state held. |
| `squire_job_verdict_confidence` | gauge | `0` none, `1` low, `2` medium, `3` high. |
| `squire_job_gpu_memory_peak_bytes` | gauge | Peak framebuffer used on a single GPU. |
| `squire_job_gpu_memory_capacity_bytes` | gauge | That GPU's total framebuffer. |
| `squire_job_gpus_held` | gauge | GPU devices allocated to the job. |
| `squire_job_gpus_lit` | gauge | Devices that have done work at some point in the run. |
| `squire_job_zombie` | gauge | Kept for compatibility; derived from `squire_job_activity`. |
| `squire_pending_gpu_jobs` | gauge | Jobs waiting because the cluster is short of GPUs. No job labels. |
| `squire_pending_gpus` | gauge | Devices those waiting jobs are asking for. No job labels. |

`squire_job_gpus_lit` is emitted **only when measured**. A Prometheus series cannot say "unknown", so an unread count is left out entirely rather than published as `0` — a dashboard averaging it would otherwise show idle devices that were never measured. Compare it against `squire_job_gpus_held` for the same job; the gap is the idle allocation.

The two `squire_pending_*` series carry no job labels, because queue pressure is a fact about the cluster rather than about any one job. Both are **always emitted, including zero** — a dashboard has to be able to tell "nobody is waiting" from "Squire is not running", and only one of those is worth an alert.

## Design principles

1. **Trust is the product.** A tool that cries wolf gets muted. Every default leans toward under-flagging.
2. **Never judge on a snapshot.** Peak over a window, past a grace period.
3. **Assert what you measure; hedge what you infer.** Over-provisioned is stated plainly. Possibly-under-provisioned is only ever hinted.
4. **Measure the right thing, or say you couldn't.** Telemetry is scoped to the exact devices a job holds; where that is not possible, the output marks it.
5. **A measured zero and an unread value are different facts.** Nothing collapses them. A dash is not a nought.
6. **Defaults are the product; configuration is an escape hatch.** Global overrides only — no per-job knobs.
7. **Augment Slurm; never do its scheduling.** Squire states one thing Slurm structurally cannot see: that an allocation is doing no real work. Where Slurm already knows, Squire reports what Slurm says and names the setting that fixes it.
8. **Never assume naming.** Nothing keys on a name a site can choose for itself. "Is this a GPU job" is `gres/gpu` in `TresAlloc`; Slurm-node to pod is the operator's own label, not string surgery.
9. **Judge only what can still change.** Finished jobs are left alone. A job that has exited cannot be told to set a time limit.
10. **Read-only.** Observation and reporting touch nothing on the cluster.
