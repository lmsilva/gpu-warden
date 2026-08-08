# gpu-warden
Finds GPUs that Slurm has allocated but are not actively used. Joins slurmrestd, DCGM, and Kubernetes pod identity into per-job efficiency metrics and Events.

## Setup a dev environment
```
source ./env.sh
./scripts/setup.sh
./scripts/dev-tunnels.sh && source .warden-env
(setup port forwarding for slurm rest API, prometheus and grafana, mint a new JWT Token)
```

## Using GPU Warden

### Submit GPU workload
#### submit a real GPU job
e.g. sbatch train_gpt.sbatch
#### submit a zombie GPU job
e.g. sbatch --gres=gpu:1 --job-name=zombie --wrap="sleep 3600"
#### check queue and verify they are running
squeue -o '%.8i %.10j %.8T %.10M %.10R %b'

### Running GPU Warden
#### Basic Usage
```
lmsilva@TrashPanda:~/gpu-warden$ go run ./cmd/warden
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  WASTED-GPU-H  VERDICT
114    tinygpt  uid:50000  1     38m0s    97    100    0.0           ok
115    zombie   uid:50000  1     37m0s    0     0      0.6           ZOMBIE
```
lmsilva@TrashPanda:~/gpu-warden$

#### Watch it and estimate wasted dollar amount
```
lmsilva@TrashPanda:~/gpu-warden$ go run ./cmd/warden --watch 30s --dollar-rate 0.53

=== 16:10:33 ===
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  WASTED-GPU-H  VERDICT
114    tinygpt  uid:50000  1     39m0s    97    100    0.0 ($0.01)   ok
115    zombie   uid:50000  1     38m0s    0     0      0.6 ($0.34)   ZOMBIE
```

#### Serve Prometheus metrics endpoint

Do note anyone who can reach this port gets the metrics, and they include usernames. Bind it to localhost or a cluster-internal Service.
Responses are cached for `--serve-cache` (30s by default). Keep it under your Prometheus scrape interval, or you'll scrape the same numbers twice. `--serve-cache 0` turns it off and rebuilds on every scrape.

```
lmsilva@TrashPanda:~/gpu-warden$ go run ./cmd/warden --serve :9410 &
serving /metrics on :9410
lmsilva@TrashPanda:~/gpu-warden$ curl -s http://localhost:9410/metrics
# HELP warden_job_gpu_utilization_percent Average GPU utilization per running job.
# TYPE warden_job_gpu_utilization_percent gauge
warden_job_gpu_utilization_percent{job_id="114",user="",partition="all"} 97.49
warden_job_gpu_utilization_percent{job_id="115",user="",partition="all"} 0.00
# HELP warden_job_gpu_hours_wasted Allocated-but-unused GPU-hours per running job.
# TYPE warden_job_gpu_hours_wasted gauge
warden_job_gpu_hours_wasted{job_id="114",user=""} 0.02
warden_job_gpu_hours_wasted{job_id="115",user=""} 0.66
# HELP warden_job_zombie 1 if the job is judged a zombie allocation.
# TYPE warden_job_zombie gauge
warden_job_zombie{job_id="114",user=""} 0
warden_job_zombie{job_id="115",user=""} 1
lmsilva@TrashPanda:~/gpu-warden$
```

#### Act on the findings by stamping the POD!
```
lmsilva@TrashPanda:~/gpu-warden$ go run ./cmd/warden --watch 30s --dollar-rate 0.53 --act
event emitted on slurm-worker-gpu-1 for job 115

=== 16:12:48 ===
JOBID  NAME     USER       GPUS  ELAPSED  AVG%  PEAK%  WASTED-GPU-H  VERDICT
114    tinygpt  uid:50000  1     41m0s    98    100    0.0 ($0.01)   ok
115    zombie   uid:50000  1     41m0s    0     0      0.7 ($0.36)   ZOMBIE

lmsilva@TrashPanda:~/gpu-warden$ ZNODE=$(kubectl -n slurm exec slurm-controller-0 -c slurmctld -- squeue -h -n zombie -o %N)
lmsilva@TrashPanda:~/gpu-warden$ ZPOD=$(kubectl -n slurm get pods -l nodeset.slinky.slurm.net/pod-hostname=$ZNODE -o jsonpath='{.items[0].metadata.name}')
lmsilva@TrashPanda:~/gpu-warden$ kubectl -n slurm describe pod "$ZPOD" | tail -n 15
QoS Class:       BestEffort
Node-Selectors:  kubernetes.io/os=linux
                 workload=gpu
Tolerations:     node.kubernetes.io/not-ready:NoExecute op=Exists for 300s
                 node.kubernetes.io/unreachable:NoExecute op=Exists for 300s
                 nvidia.com/gpu=present:NoSchedule
                 nvidia.com/gpu:NoSchedule op=Exists
                 slinky.slurm.net/managed-node=slurm-bridge-scheduler:NoExecute
Events:
  Type     Reason             Age   From        Message
  ----     ------             ----  ----        -------
  Warning  GPUAllocationIdle  54m   gpu-warden  slurm job 113 (user uid:50000) holds GPUs with no activity: peak 0% over 15m0s
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
| `--serve` | off | expose `/metrics` on this address instead of printing a table |
| `--serve-cache` | `30s` | how long a build is reused before `/metrics` rebuilds (0 disables) |
| `--act` | `false` | emit Kubernetes Events for zombie findings |
| `--zombie-threshold` | `5` | GPU util % below which a job may be a zombie |
| `--zombie-window` | `15m` | how long utilization must stay low before judging |
| `--dollar-rate` | `0` | $/GPU-hour, for costing the waste column |

Environment variables: `SLURM_JWT` for the slurmrestd token, and `WARDEN_SLURM_URL`, `WARDEN_SLURM_API`, `WARDEN_PROM_URL`, `WARDEN_NAMESPACE`, `WARDEN_POD_HOSTNAME_LABEL`, `WARDEN_POD_LABEL` as defaults for the flags above.

In `--serve` mode, warden honours the scrape timeout Prometheus sends and finishes just inside it, so a slow cluster gets an error you can read instead of a dropped connection.