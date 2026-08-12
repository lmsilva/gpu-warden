# squire
Finds GPUs that Slurm has allocated but are not actively used. Joins slurmrestd, DCGM, and Kubernetes pod identity into per-job efficiency metrics and Events.

---

## The verdict model

Squire produces **two independent verdicts per job**, because "is this job wasting a GPU" is really two different questions.

**Activity — what the GPU is doing.** A lifecycle, not a flag:

| State | Meaning |
|---|---|
| `analyzing` | Inside the warmup window, or not enough telemetry to judge. **The safe default** — Squire says "not enough data" rather than guessing. |
| `healthy` | Real GPU work observed. |
| `idle` | Sustained near-zero work past warmup. A measurement, not an accusation. |
| `zombie` | Idle **plus** evidence it isn't coming back (long duration, or the crash signature). |

**Sizing — whether the job asked for the right amount of GPU.** Judged only for `healthy` jobs, since an idle job's waste is already reported by Activity:

| State | Meaning |
|---|---|
| `over-provisioned` | **Asserted.** Peak memory over the whole run never approached the card's size, *and* the job was not working the card hard. Both, because a compute-bound job with a small footprint is correctly sized. |
| `right-sized` | No sizing flag fired. |
| `possibly under-provisioned` | **Hinted only.** A maxed-out card can mean healthy-and-perfect or starving-for-more, so Squire never asserts this. |

Every verdict carries a **confidence** (`high` / `medium` / `low`) and the evidence behind it in plain English.

**Idle → Zombie promotion.** Idle becomes Zombie when either is true:

- **Duration** — idle has persisted past `--zombie-after` (default 2h).
- **The crash signature** — the job's lifetime average proves it did real work, but its recent peak is flat. The training died; the allocation lived on.

**Why "no burst ever" is not a detection gap.** Warmup ends at a fixed ceiling *or* the first real burst, whichever comes first. The burst only ever ends warmup *early*, for honest jobs. A true zombie never bursts, so warmup ends at the ceiling and the idle window then counts against it. The zombie convicts itself by never bursting.

---

## Per-job attribution

Telemetry is scoped to the **exact GPU devices a job holds**, read from Slurm's `gres_detail`, not to every GPU on its nodes. On a shared node that difference is the whole verdict: an idle job reading its neighbour's utilization was measured at 97% against a real Prometheus when it was doing nothing at all.

Where Slurm does not publish `gres_detail`, Squire falls back to node-wide scoping and **marks the row with `*`** plus a footnote. It never quietly presents node-wide numbers as if they were the job's own.

Note: Measured on a shared 4-GPU node: two jobs, one pod, one holding GPUs 0-1 and the other GPUs 2-3. DCGM read 0% on the first pair and 100% on the second at the same moment, and Squire reported each job only its own. Scoped by pod alone, both jobs would have read 100%.

---

## The signals Squire reads, and why

**DCGM** is NVIDIA's Data Center GPU Manager — the telemetry stack that publishes per-GPU readings. Squire reads them from Prometheus via *dcgm-exporter*; it never touches a GPU directly.

| Signal | DCGM field | Unit | Why Squire reads it |
|---|---|---|---|
| GPU utilization | `DCGM_FI_DEV_GPU_UTIL` | percent, 0–100 | The number everyone watches — and a **time-based occupancy flag, not a work measurement**. One tiny kernel on one of a hundred-plus compute units reads as 100%.  |
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
| `--grace` | 15m | Warmup ceiling: covers data loading, checkpoint restore, kernel compile. |
| `--burst-util` | 15 | Utilization (%) counting as real work, ending warmup early. |
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

## Using GPU Squire

### Submit GPU workload
#### submit a real GPU job
e.g. sbatch train_gpt.sbatch
#### submit a zombie GPU job
e.g. sbatch --gres=gpu:1 --job-name=zombie --wrap="sleep 3600"
#### check queue and verify they are running
squeue -o '%.8i %.10j %.8T %.10M %.10R %b'

### Running GPU Squire
#### Basic Usage
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM        WASTED-GPU-H  ACTIVITY        SIZING
129    tinygpt  uid:50000  1     1m0s     100   100    2.9/15G (20%)  0.0           healthy:medium  right-sized
130    zombie   uid:50000  1     1m0s     0     0      0.0/15G (0%)   0.0           analyzing       -
lmsilva@PANDAMONIUM:~/squire$
```

#### Show the evidence
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --wide
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM        WASTED-GPU-H  ACTIVITY        SIZING       WHY
129    tinygpt  uid:50000  1     2m0s     100   100    2.9/15G (20%)  0.0           healthy:medium  right-sized  GPU util peak 100% over 30m0s; tensor cores near idle (0%) - util may not mean training throughput
130    zombie   uid:50000  1     2m0s     0     0      0.0/15G (0%)   0.0           analyzing       -            warming up (2m0s of 15m0s, no real burst yet)
lmsilva@PANDAMONIUM:~/squire$
```

#### Watch it and estimate wasted dollar amount
```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --watch 30s --dollar-rate 0.53 -wide

=== 10:26:09 ===
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  GPU-MEM        WASTED-GPU-H  ACTIVITY        SIZING       WHY
129    tinygpt  uid:50000  1     3m0s     99    100    2.9/15G (20%)  0.0 ($0.00)   healthy:medium  right-sized  GPU util peak 100% over 30m0s; tensor cores near idle (0%) - util may not mean training throughput
130    zombie   uid:50000  1     2m0s     0     0      0.0/15G (0%)   0.0 ($0.02)   analyzing       -            warming up (2m0s of 15m0s, no real burst yet)
```

#### Serve Prometheus metrics endpoint

Do note anyone who can reach this port gets the metrics, and they include usernames. Bind it to localhost or a cluster-internal Service.
Responses are cached for `--serve-cache` (30s by default). Keep it under your Prometheus scrape interval, or you'll scrape the same numbers twice. `--serve-cache 0` turns it off and rebuilds on every scrape.

```
lmsilva@PANDAMONIUM:~/squire$ go run ./cmd/squire --serve :9410 & 
serving /metrics on :9410 
lmsilva@PANDAMONIUM:~/squire$ curl -s http://localhost:9410/metrics | grep -E 'squire_job_(activity|sizing)'
# HELP squire_job_activity Job activity verdict: 1 on the state currently held (analyzing, healthy, idle, zombie).
# TYPE squire_job_activity gauge
squire_job_activity{job_id="129",user="uid:50000",partition="all",state="analyzing"} 0
squire_job_activity{job_id="129",user="uid:50000",partition="all",state="healthy"} 1
squire_job_activity{job_id="129",user="uid:50000",partition="all",state="idle"} 0
squire_job_activity{job_id="129",user="uid:50000",partition="all",state="zombie"} 0
squire_job_activity{job_id="130",user="uid:50000",partition="all",state="analyzing"} 1
squire_job_activity{job_id="130",user="uid:50000",partition="all",state="healthy"} 0
squire_job_activity{job_id="130",user="uid:50000",partition="all",state="idle"} 0
squire_job_activity{job_id="130",user="uid:50000",partition="all",state="zombie"} 0
# HELP squire_job_sizing Job sizing verdict: 1 on the state currently held (unknown, right_sized, over_provisioned, under_provisioned).
# TYPE squire_job_sizing gauge
squire_job_sizing{job_id="129",user="uid:50000",partition="all",state="unknown"} 0
squire_job_sizing{job_id="129",user="uid:50000",partition="all",state="right_sized"} 1
squire_job_sizing{job_id="129",user="uid:50000",partition="all",state="over_provisioned"} 0
squire_job_sizing{job_id="129",user="uid:50000",partition="all",state="under_provisioned"} 0
squire_job_sizing{job_id="130",user="uid:50000",partition="all",state="unknown"} 1
squire_job_sizing{job_id="130",user="uid:50000",partition="all",state="right_sized"} 0
squire_job_sizing{job_id="130",user="uid:50000",partition="all",state="over_provisioned"} 0
squire_job_sizing{job_id="130",user="uid:50000",partition="all",state="under_provisioned"} 0
lmsilva@PANDAMONIUM:~/squire$
```

#### Act on the findings by stamping the POD!
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
lmsilva@PANDAMONIUM:~/squire$ ZNODE=$(kubectl -n slurm exec slurm-controller-0 -c slurmctld -- squeue -h -n zombie -o %N)
lmsilva@PANDAMONIUM:~/squire$ ZPOD=$(kubectl -n slurm get pods -l nodeset.slinky.slurm.net/pod-hostname=$ZNODE -o jsonpath='{.items[0].metadata.name}')
lmsilva@PANDAMONIUM:~/squire$ kubectl -n slurm describe pod "$ZPOD" | tail -n 15
    Type:        EmptyDir (a temporary directory that shares a pod's lifetime)
    Medium:      Memory
    SizeLimit:   <unset>
QoS Class:       BestEffort
Node-Selectors:  kubernetes.io/os=linux
                 workload=gpu
Tolerations:     node.kubernetes.io/not-ready:NoExecute op=Exists for 300s
                 node.kubernetes.io/unreachable:NoExecute op=Exists for 300s
                 nvidia.com/gpu=present:NoSchedule
                 nvidia.com/gpu:NoSchedule op=Exists
                 slinky.slurm.net/managed-node=slurm-bridge-scheduler:NoExecute
Events:
  Type     Reason             Age    From        Message
  ----     ------             ----   ----        -------
  Warning  GPUAllocationIdle  2m23s  squire  slurm job 130 (user uid:50000) holds GPUs with no activity: confidence high: idle 4m0s (peak GPU 0% over 30m0s)
lmsilva@PANDAMONIUM:~/squire$
```

## Flags

| Flag | Default | What it does |
|---|---|---|
| `--slurm-url` | `http://localhost:6820` | slurmrestd base URL |
| `--slurm-api` | `v0.0.44` | slurmrestd API version |
| `--prom-url` | `http://localhost:9090` | Prometheus base URL |
| `--kubeconfig` | `$KUBECONFIG`, else `~/.kube/config` | kubeconfig path |
| `--namespace` | `slurm` | namespace holding the Slurm worker pods |
| `--pod-hostname-label` | `nodeset.slinky.slurm.net/pod-hostname` | pod label carrying the Slurm node name |
| `--pod-label` | `exported_pod` | DCGM metric label carrying the pod name |
| `--watch` | `0` | refresh interval for top mode (0 = print once) |
| `--wide` | `false` | show the evidence behind each verdict |
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

`--zombie-threshold` and `--zombie-window` were removed. They were named after the one verdict they produced; the rules they governed are now `--idle-util` and `--idle-window`. Passing a removed flag exits 2 with `flag provided but not defined`.

Environment variables: `SLURM_JWT` for the slurmrestd token, and `SQUIRE_SLURM_URL`, `SQUIRE_SLURM_API`, `SQUIRE_PROM_URL`, `SQUIRE_NAMESPACE`, `SQUIRE_POD_HOSTNAME_LABEL`, `SQUIRE_POD_LABEL` as defaults for the flags above.

In `--serve` mode, Squire honours the scrape timeout Prometheus sends and finishes just inside it, so a slow cluster gets an error you can read instead of a dropped connection.

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
| `squire_job_zombie` | gauge | Kept for compatibility; derived from `squire_job_activity`. |

## Design principles

1. **Trust is the product.** A tool that cries wolf gets muted. Every default leans toward under-flagging.
2. **Never judge on a snapshot.** Peak over a window, past a grace period.
3. **Assert what you measure; hedge what you infer.** Over-provisioned is stated plainly. Possibly-under-provisioned is only ever hinted.
4. **Measure the right thing, or say you couldn't.** Telemetry is scoped to the exact devices a job holds; where that is not possible, the output marks it.
5. **Defaults are the product; configuration is an escape hatch.** Global overrides only — no per-job knobs.
6. **Augment Slurm; never do its scheduling.** Squire states one thing Slurm structurally cannot see: that an allocation is doing no real work.
7. **Never assume naming.** Nothing keys on a name a site can choose for itself. "Is this a GPU job" is `gres/gpu` in `TresAlloc`; Slurm-node to pod is the operator's own label, not string surgery.
8. **Read-only.** Observation and reporting touch nothing on the cluster.
