package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/lmsilva/squire/internal/buildinfo"
	"github.com/lmsilva/squire/internal/cycle"
	"github.com/lmsilva/squire/internal/expose"
	"github.com/lmsilva/squire/internal/kube"
	"github.com/lmsilva/squire/internal/promapi"
	"github.com/lmsilva/squire/internal/report"
	"github.com/lmsilva/squire/internal/slurmapi"
	"github.com/lmsilva/squire/internal/slurmcfg"
	"github.com/lmsilva/squire/internal/verdict"
	"github.com/lmsilva/squire/internal/view"
)

// cycleTimeout bounds one build of the reports, in either mode. The server's
// WriteTimeout must stay comfortably above it, or a slow cluster would be cut
// off mid-response rather than returning an honest 500.
const cycleTimeout = 30 * time.Second

// scrapeTimeoutOffset is subtracted from the scrape timeout Prometheus sends,
// so the build finishes and the response is written before the client gives
// up. Without it, network latency alone can push the reply past the deadline.
const scrapeTimeoutOffset = 500 * time.Millisecond

// scrapeBudget returns how long this request's build may take. Prometheus
// sends the scrape timeout it is using; finishing inside it means returning a
// readable error rather than having the connection cut. Anything without the
// header - a curl, a health check - gets cycleTimeout.
func scrapeBudget(r *http.Request) time.Duration {
	v := r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds")
	if v == "" {
		return cycleTimeout
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs <= 0 {
		return cycleTimeout
	}
	full := time.Duration(secs * float64(time.Second))
	// A timeout shorter than the offset would leave nothing, or a negative
	// budget that expires instantly. Use it whole and accept the tight fit.
	if full <= scrapeTimeoutOffset {
		return full
	}
	return full - scrapeTimeoutOffset
}

type config struct {
	// Config carries the Slurm connection settings, bound from the same
	// place the configuration check binds them so the two cannot drift.
	slurmcfg.Config

	promURL    string
	kubeconfig string
	namespace  string
	serveAddr  string
	serveCache time.Duration
	watch      time.Duration
	act        bool
	dollarRate float64
	wide       bool

	// th holds the verdict thresholds. They are a single struct rather than
	// loose fields so the whole opinion travels together into the Builder
	th verdict.Thresholds

	podHostLabel   string // POD label carrying the Slurm node name
	podMetricLabel string // DCGM SERIES label carrying the pod name
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseConfig builds the configuration for the monitoring modes from args,
// which excludes the program name. Taking the arguments rather than reading
// os.Args means each mode parses its own list and nothing rewrites a global.
//
// The configuration check is a separate binary with its own, much smaller set.
// Sharing one would mean its help listing thresholds and Kubernetes settings it
// never reads, and linking a Kubernetes client it never calls.
func parseConfig(args []string) config {
	var c config
	fs := flag.NewFlagSet("squire", flag.ExitOnError)
	slurmcfg.Bind(fs, &c.Config)
	// Defaults come from the verdict package, so the flag help text and the
	// compiled-in opinion can never drift apart.
	def := verdict.DefaultThresholds()

	fs.StringVar(&c.promURL, "prom-url", envOr("SQUIRE_PROM_URL", "http://localhost:9090"), "Prometheus base URL")
	fs.StringVar(&c.kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "kubeconfig path (default: in-cluster if in a pod, else ~/.kube/config)")
	fs.StringVar(&c.namespace, "namespace", envOr("SQUIRE_NAMESPACE", "slurm"), "namespace of Slurm worker pods")
	// podHostLabel is on the POD (Kubernetes) and carries the Slurm node name.
	// podMetricLabel is on the DCGM SERIES (Prometheus) and carries the pod name.
	fs.StringVar(&c.podHostLabel, "pod-hostname-label", envOr("SQUIRE_POD_HOSTNAME_LABEL", "nodeset.slinky.slurm.net/pod-hostname"), "pod label carrying the Slurm node name")
	fs.StringVar(&c.podMetricLabel, "pod-label", envOr("SQUIRE_POD_LABEL", "exported_pod"), "DCGM metric label carrying the pod name")
	fs.StringVar(&c.serveAddr, "serve", "", "if set (e.g. :9410), expose /metrics instead of printing a table")
	fs.DurationVar(&c.serveCache, "serve-cache", 30*time.Second, "minimum age of a cached build before /metrics rebuilds (0 disables)")
	fs.DurationVar(&c.watch, "watch", 0, "refresh interval for top mode (0 = print once)")
	fs.BoolVar(&c.act, "act", false, "emit Kubernetes Events for zombie findings")
	fs.Float64Var(&c.dollarRate, "dollar-rate", 0, "optional $/GPU-hour for waste costing")
	fs.BoolVar(&c.wide, "wide", false, "show the evidence behind each verdict")

	// Threshold overrides. These are GLOBAL, per Squire instance — there is
	// deliberately no per-job knob, so nobody can tune away an inconvenient
	// verdict on their own job.
	fs.DurationVar(&c.th.GraceCeiling, "grace", def.GraceCeiling, "warmup ceiling before a job can be judged")
	fs.Float64Var(&c.th.BurstUtilPct, "burst-util", def.BurstUtilPct, "GPU util % counting as real work, ending warmup early")
	fs.DurationVar(&c.th.IdleWindow, "idle-window", def.IdleWindow, "trailing window peak utilization is measured over")
	fs.Float64Var(&c.th.IdleUtilPct, "idle-util", def.IdleUtilPct, "peak GPU util % below which the job is idle")
	fs.DurationVar(&c.th.ZombieAfter, "zombie-after", def.ZombieAfter, "how long idle must persist before it is a zombie")
	fs.Float64Var(&c.th.WorkedAvgPct, "worked-util", def.WorkedAvgPct, "avg GPU util % proving the job did real work earlier")
	fs.Float64Var(&c.th.MemFloorFrac, "mem-floor", def.MemFloorFrac, "peak memory fraction below which a job is over-provisioned")
	fs.Parse(args)

	// flag stops at the first non-flag argument and silently ignores the rest,
	// so a stray word would discard every flag after it. There are no
	// positional arguments here, so anything left over is a mistake.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "squire: unexpected argument %q\n", fs.Arg(0))
		os.Exit(2)
	}

	c.th.BurstMemFrac = def.BurstMemFrac
	c.th.UnderUtilPct = def.UnderUtilPct
	c.th.UnderMemFrac = def.UnderMemFrac

	return c
}

func main() {
	// Answered before anything is parsed or built: a binary that cannot start
	// should still be able to say what it is.
	if buildinfo.Asked(os.Args[1:]) {
		fmt.Println(buildinfo.String("squire"))
		return
	}

	c := parseConfig(os.Args[1:])

	sc := slurmapi.NewClient(c.URL, c.Version, c.Token)
	pc := promapi.NewClient(c.promURL)
	kc, err := kube.NewClient(c.kubeconfig, c.namespace, c.podHostLabel)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}

	b := &report.Builder{
		Jobs:       sc,
		Prom:       pc,
		Nodes:      kc,
		PodLabel:   c.podMetricLabel,
		Thresholds: c.th,
	}

	// One event log for the process lifetime, shared by every cycle.
	ev := newEventLog()

	// Every mode reads through the same source, so the table and the
	// exporter can only ever differ in how they render what it returned.
	src := cycle.SourceFunc(func(ctx context.Context) (cycle.Snapshot, error) {
		return runCycle(ctx, b, kc, c, ev)
	})

	// --serve turns Squire into an exporter.
	if c.serveAddr != "" {
		if c.watch > 0 {
			fmt.Println("note: --watch is ignored with --serve; Prometheus sets the cadence")
		}
		// Only the served path is cached. A --watch run asks on its own
		// cadence and a one-shot run asks once, so neither can amplify -
		// and a cache there would hand back numbers older than the
		// interval the operator asked for.
		cached := cycle.NewCache(src, c.serveCache)

		// A private mux, not http.DefaultServeMux. The default mux is process
		// global, so any dependency registering a handler in its init would be
		// served on this port too.
		mux := http.NewServeMux()
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			// Derive from the request, not Background: a scrape Prometheus
			// abandons cancels Squire's in-flight calls instead of leaving them
			// to finish into a closed connection.
			ctx, cancel := context.WithTimeout(r.Context(), scrapeBudget(r))
			defer cancel()
			got, err := cached.Snapshot(ctx)
			if err != nil {
				// 500 rather than a partial body: Prometheus will mark the target
				// down instead of thinking no jobs are running if we do not return anything
				http.Error(w, fmt.Sprintf("building reports: %v", err), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			expose.Write(w, got)
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
		got, err := src.Snapshot(cycleCtx)
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
			view.Table(os.Stdout, got, view.TableOptions{
				Wide: c.wide, DollarRate: c.dollarRate,
			})
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

// runCycle runs one pass and, when --act is set, emits Events. Both output
// modes go through this single function, which is what lets --serve and --act
// compose without duplicating the decision logic.
func runCycle(ctx context.Context, b *report.Builder, kc *kube.Client, c config, ev *eventLog) (cycle.Snapshot, error) {
	s, err := cycle.Build(ctx, b, c.th.GraceCeiling)
	if err != nil {
		return cycle.Snapshot{}, err
	}
	if c.act {
		if err := actOnZombies(ctx, kc, s.Reports, c.th.IdleWindow, ev); err != nil {
			return s, fmt.Errorf("acting on zombies: %w", err)
		}
	}
	return s, nil
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
