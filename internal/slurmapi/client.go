// Package slurmapi is a minimal slurmrestd client for Squire's needs.
package slurmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// NoVal mirrors slurmrestd's {"set":bool,"infinite":bool,"number":N} numbers.
//
// It also accepts a bare JSON number, because slurmrestd is inconsistent about
// which fields get wrapped and that varies by API version. Declaring the wrong
// shape does not fail gracefully: encoding/json aborts the entire response
// decode, so one mis-typed field blanks every job. Accepting both removes a
// whole class of version-drift breakage.
type NoVal struct {
	Set      bool
	Infinite bool
	Number   int64
}

func (n *NoVal) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] != '{' { // bare number, e.g. 50000
		v, err := strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return fmt.Errorf("parsing number %s: %w", b, err)
		}
		n.Set, n.Number = true, v
		return nil
	}
	var wrapped struct {
		Set      bool  `json:"set"`
		Infinite bool  `json:"infinite"`
		Number   int64 `json:"number"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return fmt.Errorf("parsing wrapped number: %w", err)
	}
	n.Set, n.Infinite, n.Number = wrapped.Set, wrapped.Infinite, wrapped.Number
	return nil
}

type Job struct {
	JobID     int      `json:"job_id"`
	Name      string   `json:"name"`
	UserName  string   `json:"user_name"`
	UserID    NoVal    `json:"user_id"`
	Partition string   `json:"partition"`
	State     []string `json:"job_state"`
	Nodes     string   `json:"nodes"` // Slurm hostlist, e.g. "gpu-[0-1]"

	// GPU allocation appears in three fields with three spellings, and which
	// are populated varies by API version. See GPUCount in parse.go.
	TresAlloc   string   `json:"tres_alloc_str"` // totals inc. mem; NO gres on v0.0.44
	TresPerNode string   `json:"tres_per_node"`  // e.g. "gres/gpu:1" (per node)
	GresDetail  []string `json:"gres_detail"`    // e.g. ["gpu:1(IDX:0)"], one per node

	StartTime NoVal `json:"start_time"`

	// TimeLimit is in MINUTES, and carries its own infinite flag - a job with
	// no limit and a job with an unlimited one are different mistakes.
	TimeLimit NoVal `json:"time_limit"`

	// Shared carries the exclusivity mode. There is no "exclusive" field on
	// the job-info schema: --exclusive renders here as "none", meaning the
	// node is shared with nothing.
	Shared []string `json:"shared"`

	// StateReason is Slurm's own word for why a pending job is pending, e.g.
	// "Resources", "Priority", "DependencyNeverSatisfied".
	StateReason string `json:"state_reason"`
}

// Exclusive reports whether the job asked for whole-node allocation.
func (j Job) Exclusive() bool {
	for _, s := range j.Shared {
		if s == "none" {
			return true
		}
	}
	return false
}

// BaseState is the job's state without the flags Slurm appends after it.
func (j Job) BaseState() string {
	if len(j.State) == 0 {
		return ""
	}
	return j.State[0]
}

type jobsResponse struct {
	Jobs []Job `json:"jobs"`
}

// Node is what a node actually is. Gres is the node's own GRES string, which
// is the only honest answer to "is this a GPU node" - never its name.
type Node struct {
	Name       string   `json:"name"`
	Gres       string   `json:"gres"`
	Partitions []string `json:"partitions"`

	// RealMemory is the node's memory in MB. Declared as a NoVal because
	// slurmrestd sends it bare here but wraps the neighbouring memory fields,
	// and which is which has changed between versions.
	RealMemory NoVal `json:"real_memory"`
}

type nodesResponse struct {
	Nodes []Node `json:"nodes"`
}

// Partition nests its limits under "maximums".
type Partition struct {
	Name     string `json:"name"`
	Maximums struct {
		Time NoVal `json:"time"` // minutes
	} `json:"maximums"`
}

type partitionsResponse struct {
	Partitions []Partition `json:"partitions"`
}

type Client struct {
	base, apiVer, token string
	http                *http.Client
}

func NewClient(base, apiVer, token string) *Client {
	return &Client{base: base, apiVer: apiVer, token: token,
		http: &http.Client{Timeout: 15 * time.Second}}
}

// get fetches and decodes one slurmrestd collection. Factored out because
// every endpoint differs only in path and response shape.
func (c *Client) get(ctx context.Context, path string, out any) error {
	u := fmt.Sprintf("%s/slurm/%s/%s", c.base, c.apiVer, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("building %s request: %w", path, err)
	}
	req.Header.Set("X-SLURM-USER-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("calling slurmrestd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slurmrestd status %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding %s: %w", path, err)
	}
	return nil
}

// ListJobs returns every job slurmrestd knows about, pending included. The
// configuration checks judge submitted work, not only running work.
func (c *Client) ListJobs(ctx context.Context) ([]Job, error) {
	var jr jobsResponse
	if err := c.get(ctx, "jobs", &jr); err != nil {
		return nil, err
	}
	return jr.Jobs, nil
}

// ListNodes returns every node, so a check can read what a node actually has.
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var nr nodesResponse
	if err := c.get(ctx, "nodes", &nr); err != nil {
		return nil, err
	}
	return nr.Nodes, nil
}

// ListPartitions returns every partition and its limits.
func (c *Client) ListPartitions(ctx context.Context) ([]Partition, error) {
	var pr partitionsResponse
	if err := c.get(ctx, "partitions", &pr); err != nil {
		return nil, err
	}
	return pr.Partitions, nil
}

// ListRunningJobs returns jobs currently in the RUNNING state. Slurm has no
// server-side filter here, so the whole list is fetched and narrowed.
func (c *Client) ListRunningJobs(ctx context.Context) ([]Job, error) {
	jobs, err := c.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	running := make([]Job, 0, len(jobs))
	for _, j := range jobs {
		if j.BaseState() == "RUNNING" {
			running = append(running, j)
		}
	}
	return running, nil
}
