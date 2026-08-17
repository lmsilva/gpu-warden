// Package act emits Kubernetes Events for zombie allocations.
//
// It is the only thing Squire writes to a cluster, and it is behind --act.
// Everything else reads. That is worth keeping in one package with a name
// that says so: a reviewer asking "what does this touch" has one file to
// read.
//
// The Event goes on the worker pod holding the job's first allocated node,
// so it appears in kubectl describe beside everything else that happened
// there, with no dashboard to build.
package act

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lmsilva/squire/internal/kube"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
)

// Log remembers when each job was last Evented, so a job is not
// re-stamped every cycle. The mutex is required because --serve runs
// OnZombies inside an HTTP handler, and net/http runs handlers
// concurrently: a bare map here would crash the process.
type Log struct {
	mu   sync.Mutex
	seen map[int]time.Time
}

func NewLog() *Log {
	return &Log{seen: make(map[int]time.Time)}
}

// claim reports whether this job should be Evented now, and records the
// attempt if so. One method rather than a separate check and record: split
// in two, both scrapes could pass the check and both emit for one job.
//
// It records before the Event is created, so a failed emit consumes the
// window. At-most-once is the safer direction for a cluster write.
func (e *Log) claim(jobID int, win time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.seen[jobID]; ok && time.Since(last) < win {
		return false
	}
	e.seen[jobID] = time.Now()
	return true
}

// OnZombies emits one Kubernetes Event per zombie job, on the worker pod
// holding its first allocated node.
func OnZombies(ctx context.Context, kc *kube.Client, reports []report.JobReport, win time.Duration, ev *Log) error {
	nodes, err := kc.NodeMap(ctx)
	if err != nil {
		return fmt.Errorf("resolving pods for events: %w", err)
	}
	for _, r := range reports {
		if !r.IsZombie() {
			continue
		}
		slurmNodes, err := slurmapi.ExpandNodes(r.Job.Nodes)
		if err != nil || len(slurmNodes) == 0 {
			fmt.Printf("job %d: cannot expand nodes %q, skipping event\n", r.Job.JobID, r.Job.Nodes)
			continue
		}
		// Slurm node name -> pod. A node missing from the map is a worker
		// mid-restart: skip and say so, rather than crashing a monitoring tool.
		pod, ok := nodes[slurmNodes[0]]
		if !ok {
			fmt.Printf("job %d: no pod for slurm node %q, skipping event\n", r.Job.JobID, slurmNodes[0])
			continue
		}
		// Claimed after the pod is resolved, not at the top of the loop: the
		// skips above are transient, and claiming first would silence the job
		// for a whole window without ever having tried.
		if !ev.claim(r.Job.JobID, win) {
			continue // already warned about this job within the window
		}
		// The Event now carries the verdict's own words: its confidence and the
		// evidence line. Someone reading `kubectl describe pod` gets the
		// reasoning, not just an accusation.
		detail := fmt.Sprintf("confidence %s: %s", r.Verdict.Confidence, r.Verdict.Reason())
		if err := kc.EmitZombieEvent(ctx, &pod, r.Job.JobID, r.Job.Owner(), detail); err != nil {
			return err
		}
		fmt.Printf("event emitted on %s for job %d\n", pod.Name, r.Job.JobID)
	}
	return nil
}
