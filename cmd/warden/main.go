package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lmsilva/gpu-warden/internal/expose"
	"github.com/lmsilva/gpu-warden/internal/kube"
	"github.com/lmsilva/gpu-warden/internal/promapi"
	"github.com/lmsilva/gpu-warden/internal/report"
	"github.com/lmsilva/gpu-warden/internal/slurmapi"
)

// cycleTimeout bounds one build of the reports, in either mode. The server's
// WriteTimeout must stay comfortably above it, or a slow cluster would be cut
// off mid-response rather than returning an honest 500.
const cycleTimeout = 30 * time.Second

type config struct {
	slurmURL   string
	slurmVer   string
	slurmToken string
	promURL    string
	kubeconfig string
	namespace  string
	serveAddr  string
	serveCache time.Duration
	watch      time.Duration
	act        bool
	zombiePct  float64
	zombieWin  time.Duration
	dollarRate float64

	podHostLabel   string // POD label carrying the Slurm node name
	podMetricLabel string // DCGM SERIES label carrying the pod name
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func parseConfig() config {
	var c config
	flag.StringVar(&c.slurmURL, "slurm-url", envOr("WARDEN_SLURM_URL", "http://localhost:6820"), "slurmrestd base URL")
	flag.StringVar(&c.slurmVer, "slurm-api", envOr("WARDEN_SLURM_API", "v0.0.44"), "slurmrestd API version")
	flag.StringVar(&c.promURL, "prom-url", envOr("WARDEN_PROM_URL", "http://localhost:9090"), "Prometheus base URL")
	flag.StringVar(&c.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path (default: in-cluster if in a pod, else ~/.kube/config)")
	flag.StringVar(&c.namespace, "namespace", envOr("WARDEN_NAMESPACE", "slurm"), "namespace of Slurm worker pods")
	// podHostLabel is on the POD (Kubernetes) and carries the Slurm node name.
	// podMetricLabel is on the DCGM SERIES (Prometheus) and carries the pod name.
	flag.StringVar(&c.podHostLabel, "pod-hostname-label", envOr("WARDEN_POD_HOSTNAME_LABEL", "nodeset.slinky.slurm.net/pod-hostname"), "pod label carrying the Slurm node name")
	flag.StringVar(&c.podMetricLabel, "pod-label", envOr("WARDEN_POD_LABEL", "exported_pod"), "DCGM metric label carrying the pod name")
	flag.StringVar(&c.serveAddr, "serve", "", "if set (e.g. :9410), expose /metrics instead of printing a table")
	flag.DurationVar(&c.serveCache, "serve-cache", 30*time.Second, "minimum age of a cached build before /metrics rebuilds (0 disables)")
	flag.DurationVar(&c.watch, "watch", 0, "refresh interval for top mode (0 = print once)")
	flag.BoolVar(&c.act, "act", false, "emit Kubernetes Events for zombie findings")
	flag.Float64Var(&c.zombiePct, "zombie-threshold", 5, "GPU util % below which a job may be a zombie")
	flag.DurationVar(&c.zombieWin, "zombie-window", 15*time.Minute, "sustained window for zombie judgment")
	flag.Float64Var(&c.dollarRate, "dollar-rate", 0, "optional $/GPU-hour for waste costing")
	flag.Parse()

	// Slurm Rest API's JWT Token
	c.slurmToken = os.Getenv("SLURM_JWT")
	return c
}

func main() {
	c := parseConfig()

	sc := slurmapi.NewClient(c.slurmURL, c.slurmVer, c.slurmToken)
	pc := promapi.NewClient(c.promURL)
	kc, err := kube.NewClient(c.kubeconfig, c.namespace, c.podHostLabel)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

	b := &report.Builder{
		Jobs:      sc,
		Prom:      pc,
		Nodes:     kc,
		PodLabel:  c.podMetricLabel,
		ZombiePct: c.zombiePct,
		ZombieWin: c.zombieWin,
	}

	// One event log for the process lifetime, shared by every cycle.
	ev := newEventLog()

	// --serve turns warden into an exporter.
	if c.serveAddr != "" {
		if c.watch > 0 {
			fmt.Println("note: --watch is ignored with --serve; Prometheus sets the cadence")
		}
		cache := newCycleCache(c.serveCache)

		// A private mux, not http.DefaultServeMux. The default mux is process
		// global, so any dependency registering a handler in its init would be
		// served on this port too.
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			// Derive from the request, not Background: a scrape Prometheus
			// abandons cancels warden's in-flight calls instead of leaving them
			// to finish into a closed connection.
			ctx, cancel := context.WithTimeout(r.Context(), cycleTimeout)
			defer cancel()
			reports, err := cache.get(ctx, func(ctx context.Context) ([]report.JobReport, error) {
				return runCycle(ctx, b, kc, c, ev)
			})
			if err != nil {
				// 500 rather than a partial body: Prometheus will mark the target
				// down instead of thinking no jobs are running if we do not return anything
				http.Error(w, fmt.Sprintf("building reports: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			expose.Write(w, reports)
		})

		// Explicit timeouts. without them, ListenAndServe leaves all of these at zero,
		// meaning no limit, so a client sending headers slowly can hold a
		// connection open forever.
		srv := &http.Server{
			Addr:              c.serveAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      cycleTimeout + 60*time.Second,
			IdleTimeout:       60 * time.Second,
		}
		fmt.Println("serving /metrics on", c.serveAddr)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
		return // ListenAndServe only returns on failure
	}

	// Ctrl-C cancels the context rather than killing the process mid-cycle, so
	// an in-flight Slurm or Prometheus call unwinds cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// One loop that serves both modes: --watch 0 runs the body once and returns.
	for {
		cycleCtx, cancel := context.WithTimeout(ctx, cycleTimeout)
		reports, err := runCycle(cycleCtx, b, kc, c, ev)
		cancel()

		if err != nil {
			fmt.Println("error:", err)
			if c.watch == 0 {
				os.Exit(1)
			}
			// In watch mode a transient failure is not fatal: report it and
			// try again next tick. This is better than exiting on one bad scrape!
		} else {
			if c.watch > 0 {
				fmt.Printf("\n=== %s ===\n", time.Now().Format("15:04:05"))
			}
			printTable(os.Stdout, reports, c)
		}

		if c.watch == 0 {
			return
		}
		select {
		case <-time.After(c.watch):
		case <-ctx.Done():
			fmt.Println("\nstopping")
			return
		}
	}
}

// cycleCache serves the last successful build to any scrape arriving within
// minAge of it. Without it every scrape runs a full fan-out across Slurm,
// Kubernetes and Prometheus, so an unauthenticated caller in a loop amplifies
// into those services rather than into warden.
//
// sem is a one-slot channel used as a lock, held across the build on purpose:
// a scrape arriving mid-build waits, then finds the cache fresh and returns
// without querying anything. One build, however many scrapers.
//
// It is a channel rather than a sync.Mutex because a mutex cannot be given up.
// Prometheus abandons a scrape after its own timeout, which is shorter than
// cycleTimeout, and a waiter blocked on a mutex would keep waiting for a
// client that has already gone.
type cycleCache struct {
	sem     chan struct{}
	minAge  time.Duration
	at      time.Time
	reports []report.JobReport
}

// newCycleCache is required rather than optional: a nil channel blocks
// forever, so a zero-value cycleCache would hang the first scrape.
func newCycleCache(minAge time.Duration) *cycleCache {
	return &cycleCache{sem: make(chan struct{}, 1), minAge: minAge}
}

// get returns a cached build if one is younger than minAge, otherwise builds
// a fresh one. Errors are never cached: one transient Slurm hiccup must not
// buy minAge of silence.
//
// Freshness is judged by the timestamp, never by the slice. report.Build
// returns nil when no GPU jobs are running, so a c.reports != nil test would
// never cache anything on an idle cluster.
func (c *cycleCache) get(ctx context.Context, build func(context.Context) ([]report.JobReport, error)) ([]report.JobReport, error) {
	// Taking the slot is the lock. The ctx case is what a mutex cannot do:
	// a scrape whose client has given up stops waiting and returns.
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !c.at.IsZero() && time.Since(c.at) < c.minAge {
		return c.reports, nil
	}
	reports, err := build(ctx)
	if err != nil {
		return nil, err
	}
	c.at, c.reports = time.Now(), reports
	return reports, nil
}

// runCycle builds the reports and, when --act is set, emits Events. Both output
// modes go through this single function, which is what lets --serve and --act
// compose without duplicating the decision logic.
func runCycle(ctx context.Context, b *report.Builder, kc *kube.Client, c config, ev *eventLog) ([]report.JobReport, error) {
	reports, err := b.Build(ctx)
	if err != nil {
		return nil, err
	}
	if c.act {
		if err := actOnZombies(ctx, kc, reports, c.zombieWin, ev); err != nil {
			return reports, fmt.Errorf("acting on zombies: %w", err)
		}
	}
	return reports, nil
}

// printTable renders reports as a top-style table.
func printTable(out io.Writer, reports []report.JobReport, c config) {
	// tabwriter buffers: nothing prints until Flush.
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	anyPodWide := false
	fmt.Fprintln(w, "JOBID\tNAME\tUSER\tGPUS\tELAPSED\tAVG%\tPEAK%\tWASTED-GPU-H\tVERDICT")
	for _, r := range reports {
		verdict := "ok"
		if !r.HasData {
			verdict = "no-data"
		} else if r.Zombie {
			verdict = "ZOMBIE"
		}
		cost := fmt.Sprintf("%.1f", r.WastedH)
		if c.dollarRate > 0 {
			cost = fmt.Sprintf("%.1f ($%.2f)", r.WastedH, r.WastedH*c.dollarRate)
		}
		// A job whose telemetry could not be scoped to its own GPU devices is
		// marked, because on a shared node those numbers include a
		// neighbour's work. Silently printing them as if they were the job's
		// own is the failure this column exists to prevent!
		gpus := fmt.Sprintf("%d", r.GPUs)
		if !r.PerGPU {
			gpus += "*"
			anyPodWide = true
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%.0f\t%.0f\t%s\t%s\n",
			r.Job.JobID, r.Job.Name, r.Job.Owner(), gpus,
			r.Elapsed.Round(time.Minute), r.AvgUtil, r.PeakUtil, cost, verdict)
	}
	w.Flush()
	if anyPodWide {
		fmt.Fprintln(out, "\n* GPU indices unavailable (no gres_detail): telemetry covers every GPU on the job's nodes.")
	}
}

// eventLog remembers when each job was last Evented, so a job is not
// re-stamped every cycle. The mutex is required because --serve runs
// actOnZombies inside an HTTP handler, and net/http runs handlers
// concurrently: a bare map here would crash the process.
type eventLog struct {
	mu   sync.Mutex
	seen map[int]time.Time
}

func newEventLog() *eventLog {
	return &eventLog{seen: make(map[int]time.Time)}
}

// claim reports whether this job should be Evented now, and records the
// attempt if so. One method rather than a separate check and record: split
// in two, both scrapes could pass the check and both emit for one job.
//
// It records before the Event is created, so a failed emit consumes the
// window. At-most-once is the safer direction for a cluster write.
func (e *eventLog) claim(jobID int, win time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.seen[jobID]; ok && time.Since(last) < win {
		return false
	}
	e.seen[jobID] = time.Now()
	return true
}

// actOnZombies emits one Kubernetes Event per zombie job, on the worker pod
// holding its first allocated node.
func actOnZombies(ctx context.Context, kc *kube.Client, reports []report.JobReport, win time.Duration, ev *eventLog) error {
	nodes, err := kc.NodeMap(ctx)
	if err != nil {
		return fmt.Errorf("resolving pods for events: %w", err)
	}
	for _, r := range reports {
		if !r.Zombie {
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
		// for a window without ever having tried.
		if !ev.claim(r.Job.JobID, win) {
			continue // already warned about this job within the window
		}
		detail := fmt.Sprintf("peak %.0f%% over %s", r.PeakUtil, win)
		if err := kc.EmitZombieEvent(ctx, &pod, r.Job.JobID, r.Job.Owner(), detail); err != nil {
			return err
		}
		fmt.Printf("event emitted on %s for job %d\n", pod.Name, r.Job.JobID)
	}
	return nil
}
