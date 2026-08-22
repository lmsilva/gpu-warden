# Running Squire in a cluster

`squire` is operator-side: it reads Slurm, Prometheus and the Kubernetes API, and it needs credentials for all three. This is how to give it those and nothing more.

`squire-lint` needs none of this. It reads one slurmrestd URL and runs anywhere, including a login node.

## What you need

- A Kubernetes cluster running Slurm, with slurmrestd reachable as a Service.
- Prometheus scraping [dcgm-exporter](https://github.com/NVIDIA/dcgm-exporter).
- A Slurm account for Squire to authenticate as.

## The image

Published on every release for `linux/amd64` and `linux/arm64`:

```
ghcr.io/lmsilva/squire:v0.1.0
```

It carries all three binaries on a distroless base and runs as uid 65532. There is no shell in it — that is deliberate, and `kubectl exec ... -- sh` failing is the image working. Use `kubectl debug` when you need one.

Plain binaries and checksums are attached to each release too. Those are for `squire-lint` on a login node, which typically has no container runtime at all — and would not want one for a read-only check.

## Applying the manifests

**Create the Slurm token first.** The Deployment mounts it as a required volume, so applying the manifests without it gives a pod that sits in `ContainerCreating` indefinitely — not `CrashLoopBackOff`, not a pull error, and with nothing in the pod's own events once the first mount attempt has aged out. It looks like a hung cluster and it is a missing Secret. [Creating it](#creating-it) is below; come back here after.

```bash
kubectl apply -f deploy/squire.yaml
```

Five objects: a ServiceAccount, a namespaced Role and RoleBinding, a Deployment and a Service. Raw manifests rather than a chart — enough to run it and to read in one sitting.

**Check three things before applying.**

**The namespace**, which is `slurm` throughout the manifests.

**The two URLs.** They differ by chart and release name, so find them by port — 6820 is slurmrestd's default, 9090 is Prometheus's:

```bash
kubectl get svc -A | awk 'NR==1 || /6820|9090/'
```

Build each URL from the row as `http://<NAME>.<NAMESPACE>.svc:<PORT>`. The fully qualified form resolves from any namespace.

**The image tag**, which pins a released version. Applying from an unreleased branch fails with `ImagePullBackOff` and `not found` — which reads like an authentication problem and is not one. Point it at the branch build instead.

---

**Set these once.** Every command below uses them, so substitute your own values here and the rest of this page is copy-paste:

```bash
NS=slurm
RESTAPI=http://slurm-restapi.slurm.svc:6820
```

```bash
kubectl -n $NS set image deploy/squire squire=ghcr.io/lmsilva/squire:dev
```

## What Squire is granted

The whole of it, and it is worth reading before you install anything that talks to your scheduler:

| Resource | Verbs | Why |
|---|---|---|
| `pods` | `get`, `list` | The Slurm-node-to-pod bridge, which is what scopes GPU telemetry to a job's own devices |
| `events` | `create` | The only write Squire makes, and only with `--act` |

Namespaced, not cluster-wide. Nothing else, in either direction.

**The Deployment does not pass `--act`.** It is not disabled — nothing prevents you from turning it on — but it is off unless you ask for it, for the reason in [Recording findings as Events](#recording-findings-as-events) below. Drop the `events` rule from the Role entirely if you never intend to use it.

The container runs non-root with a read-only root filesystem and all capabilities dropped. Squire writes nothing to disk, so none of that costs anything.

## The Slurm token

Squire authenticates to slurmrestd with a Slurm JWT, mounted as a file and **re-read on every request**. Replacing the Secret is enough; the pod does not restart.

That is the reason for `--slurm-token-file`. A token in an environment variable is fixed for the life of the process — a process's environment cannot be changed from outside — so when it expires, every call returns `511` until somebody restarts the pod.

### Which account

**Not `root`.** Squire reads `/jobs`, `/nodes` and `/partitions`, and an ordinary account can read all three. A Slurm token carries its account's full powers rather than a read-only subset, so a root token here could cancel jobs Squire will never touch.

The exception is a site running `PrivateData=jobs`, where an ordinary account sees only its own jobs and Squire's output collapses to almost nothing — correctly, and uselessly. That is the one case for a privileged account, and it should be a decision rather than a default.

Note that `slurm` works but is a poor choice: the SlurmUser's token is exempt from `PrivateData` restrictions, so it sees every job on the cluster, and it carries that account's full powers for the same reason `root` does.

### Creating it

On a cluster running the Slurm operator, have it mint one — set `username` first:

```bash
kubectl apply -f deploy/token-slinky.yaml
```

That produces the Secret the Deployment expects. Anywhere else, create the same Secret yourself:

```bash
kubectl -n $NS create secret generic squire-slurm-token \
  --from-literal=SLURM_JWT="$(scontrol token username=squire lifespan=infinite | cut -d= -f2)"
```

### Token rotation is manual

**`deploy/token-slinky.yaml` asks for `lifetime: 8760h` — one year.** That is deliberate, and it is long because nothing renews it.

The Slurm operator's `Token` resource takes a `refresh` field, which reads like it rotates the secret on a schedule. It was not observed doing so: a token created with a ten-minute lifetime and `refresh: true` went twenty minutes without being reissued, with nothing written to the resource's status to explain why. slurmrestd does enforce expiry — a sixty-second token returns `511` after seventy-five seconds — so a short lifetime here would simply stop working, with nothing to bring it back.

A year matches what the Slurm operator's own quickstart uses for slurm-bridge, and for the same reason.

**What makes a year acceptable is the account, not the duration.** Squire reads three endpoints, an ordinary account can read all three, and the token is scoped to that account. A long-lived token that can only read is a much smaller thing than a long-lived one that can cancel jobs — which is the other half of why this should not be `root`.

Shorten it if you have something that reissues on a schedule. Squire will pick up each new token without restarting, so a shorter lifetime costs nothing on its side.

**An expired token is visible rather than silent.** `/metrics` starts failing, the readiness probe fails with it, and the pod leaves the Service — so it surfaces as a scrape target going down, not as stale numbers nobody questions.

Replacing it takes effect within one kubelet sync, about a minute, with no restart:

```bash
kubectl -n $NS patch secret squire-slurm-token \
  -p "{\"stringData\":{\"SLURM_JWT\":\"$(scontrol token username=squire lifespan=infinite | cut -d= -f2)\"}}"
```

Where the operator minted it, delete the `Token` **and** its Secret. The Secret belongs to the cluster's signing key rather than to the `Token`, so deleting the `Token` on its own leaves the old value in place and the re-created `Token` adopts it — the operator only replaces a value it can read as expired. The patch above works in every case.

### Running a branch build

`:dev` moves with every push to the branch, and the manifest sets `imagePullPolicy: Always` because of it. Without that, Kubernetes defaults to pulling only when the image is absent — correct for a version tag, wrong for a moving one: a node that has pulled `:dev` once keeps that build through every `rollout restart` and every `set image`, and the pod cheerfully reports the version it was built with.

That failure is quiet, so it is worth knowing how to see it. Every build also publishes a `sha-<commit>` tag, and the running image's digest is what settles the question:

```bash
kubectl -n $NS get pod -l app.kubernetes.io/name=squire \
  -o jsonpath='{.items[0].status.containerStatuses[0].imageID}'
```

## Configuration findings need the node and partition lists

Squire's pass now runs the configuration checks as well as the allocation ones, and some of those compare a request against the hardware — is this time limit the partition's ceiling, does any node have this many cards. Those need two more reads of slurmrestd: the node list and the partition list.

Both are optional. If either read fails, the pass still completes, the checks that only read a job's own request still run, and the page and the table say the lists could not be read. A shorter list of findings with no explanation would read as a clean cluster, which is the failure this note exists to prevent.

With `PrivateData=nodes` or `PrivateData=partitions` set, the same account rule as for jobs applies: Squire's own token must be privileged enough to see them, or those checks go quiet with the note above rather than silently.

## What identity Slurm needs

Squire never authenticates a user and never maps a name to a uid itself — it prints and filters exactly what slurmrestd sends. Four requirements follow, all on the Slurm side, all visible as symptoms in Squire when missing.

**slurmctld must be able to resolve usernames.** The controller records the owner when the job is submitted. If its container cannot see your user database — no sssd, no NSS source, a bare `/etc/passwd` — the name is never recorded, and nothing downstream can invent it.

**slurmrestd must be able to resolve them too.** It serializes the job record on the way out, and a restapi container without the same NSS view degrades the user fields even where the controller had them. In Slinky both daemons run in pods, so "the node can resolve users" is not enough — each container must.

**If `PrivateData=jobs` is set, Squire's token needs an operator account.** With that setting, slurmrestd answers each token with only that account's own jobs. Squire's account must then be `SlurmUser`, `root`, or carry `AdminLevel=Operator` in sacctmgr — otherwise every other user's jobs silently vanish from the dashboard, the metrics, and every `squire-me` except the operator's own. The symptom is an emptier cluster than `squeue` shows, with no error anywhere.

**On Slinky, `slurm.conf` changes go through Helm values.** The operator owns the file and reconciles hand-edits away, so a `PrivateData` or NSS-related change made with an editor holds only until the next sync. Set it where the operator reads it.

**The `uid:50000` you may see** in a USER column, a metric label, or an Event is Squire's fallback when Slurm sent a uid but no name — the first two requirements failing, not the third. Everything still works: `squire-me` filters on the numeric uid, so ownership is exact even while the display name is missing. Fix the NSS view in the two containers and the names come back on the next pass.

## Recording findings as Events

**Turn this on.** It is the difference between findings that wait to be scraped and findings that arrive where people already look.

With `--act`, Squire writes a Kubernetes Event onto the pod backing the Slurm node whose job it has judged a zombie. It then appears in `kubectl describe pod`, in `kubectl get events`, and in whatever already watches Events on your cluster — beside everything else that happened to that pod, with no dashboard to build and nothing to configure. A metric tells you a number is wrong; an Event tells the person looking at that pod why.

```
Events:
  Type     Reason             Age    From    Message
  ----     ------             ----   ----    -------
  Warning  GPUAllocationIdle  2m23s  squire  slurm job 130 (user uid:50000) holds GPUs with no activity: confidence high: idle 4m0s (peak GPU 0% over 30m0s)
```

**Why it is not on by default**, despite being recommended. It is the only thing Squire writes to the cluster, and everything else it does is read-only — so enabling it should be a decision somebody made rather than something that happened when they applied a manifest. It also needs the `events` rule in the Role, and a Role that only reads is an easier thing to get approved than one that writes.

**One limitation to weigh first.** The guard that stops the same job being stamped twice is held in memory, so it covers one process and no more. A long-running `--watch --act` will not re-stamp across its own cycles; a *new* process — a restarted pod, or a second one-shot run — starts with no memory of what it recorded and stamps again.

**What that actually costs, which is less than it sounds.** Kubernetes expires Events on the API server's `--event-ttl`, an hour by default. So a duplicate appears only when a new process starts inside that window while the same job is still idle. Past it the original has already been dropped, and a fresh stamp is the current state rather than a repeat. Note also that Squire creates a distinct Event per call rather than incrementing an existing one's count, so a duplicate reads as two entries rather than as `count: 2`.

Judge it against your own restart rate. A stable Deployment stamps each zombie once; one that is crash-looping will produce noise, and that is the case worth avoiding.

### Turning it on

**In the manifest**, which is where it belongs for a permanent install. `deploy/squire.yaml` ships the flag commented out beside the others — uncomment it and apply:

```yaml
            - --slurm-token-file=/etc/squire/slurm-token
            - --act
```

```bash
kubectl apply -f deploy/squire.yaml
```

**On a running Deployment**, without editing the file:

```bash
kubectl -n $NS patch deploy squire --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--act"}]'
```

**As a one-off**, from a machine with a kubeconfig and reachable Slurm and Prometheus — no cluster change, and the tidiest way to try it before committing:

```bash
squire --act --grace 1m --zombie-after 1m
```

**Permanently**, by adding the flag to the Deployment. This rolls a new pod:

```bash
kubectl -n $NS patch deploy squire --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--act"}]'
```

Confirm it took, and watch for restarts, since that is what governs duplicates:

```bash
kubectl -n $NS get deploy squire -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
kubectl -n $NS get pod -l app.kubernetes.io/name=squire -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}{"\n"}'
```

To turn it back off, remove the flag and the pod rolls again:

```bash
kubectl -n $NS patch deploy squire --type=json \
  -p '[{"op":"remove","path":"/spec/template/spec/containers/0/args/4"}]'
```

Check the index against the `args` output above before running that — a JSON patch removes by position, not by value.

**There is no shell in the image**, which is deliberate — a shell is the first thing an attacker reaches for, and Squire needs none. It does not stop you running Squire's own commands inside the pod, because `kubectl exec` invokes a binary directly and needs no shell to do it:

```bash
kubectl -n $NS exec deploy/squire -- /usr/local/bin/squire --grace 5m --wide
```

```bash
kubectl -n $NS exec deploy/squire -- /usr/local/bin/squire-lint --slurm-url $RESTAPI
```

What you cannot do is pipe, glob, or poke around the filesystem. For that, attach a shell in a throwaway container sharing the pod's namespaces:

```bash
kubectl -n $NS debug -it deploy/squire --image=busybox:1.36 --target=squire -- sh
```

## Scraping it

The Deployment serves `/metrics` on port 9101 and the Service exposes it. Responses are cached for `--serve-cache` (30s by default) — keep that under your scrape interval, or you will scrape the same numbers twice.

`/metrics` is also the readiness probe, because it is exactly what a scrape does: a pod that passes it is a pod Prometheus can use.

## Looking at it

The same port serves a web view at `/` and the same pass as a document at `/snapshot.json` — one cache behind all three surfaces, so they cannot disagree. Forward it and open it:

```bash
kubectl -n $NS port-forward svc/squire 9101:9101
```

```
http://localhost:9101/
```

**None of these surfaces is authenticated**, and that is deliberate: `/metrics` has always served usernames and job names, so a login on the page beside it would protect nothing a `curl` cannot already reach. The reasoning is in the [README](../README.md#nothing-on-this-port-is-authenticated).

**What that means for you is one rule.** A port-forward already requires Kubernetes RBAC on this namespace, so as installed above, the boundary is your cluster's. **The first time you make Squire reachable without a port-forward, put authentication in front of it** — an Ingress with auth, an oauth2 proxy, or a NetworkPolicy holding it where it is. Do not wait to be asked; an internal dashboard listing who is wasting what is the kind of URL that gets shared.

The page reloads itself on the cache interval, so a browser left open on a wall display costs one rebuild per window no matter how many are watching.

## When it does not work

**`ImagePullBackOff`.** Read whether it says `not found` or `denied` — they are unrelated. `not found` means the tag does not exist, usually a manifest pinning a version that has not been released. `denied` means the registry: a package pushed to ghcr.io is private by default.

**`CrashLoopBackOff`.** Read the log rather than guessing:

```bash
kubectl -n $NS logs deploy/squire --previous --tail=30
```

`loading kubeconfig` means it is looking for a file that will never exist in a pod. `reading the pod service account` means it took the right path and the RoleBinding is missing.

**Running but never Ready.** The container is fine and `/metrics` is failing, which is by design — the probe is a real scrape. `511` is the token. `connection refused` or `no such host` is a wrong URL. `pods is forbidden` is the Role in the wrong namespace.

**Checking what the pod sees**, without a shell:

```bash
kubectl -n $NS debug -it deploy/squire --image=busybox:1.36 --target=squire -- sh
```
