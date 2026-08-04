// Package slurmapi is a minimal slurmrestd client for warden's needs.
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
	Set    bool
	Number int64
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
		Set    bool  `json:"set"`
		Number int64 `json:"number"`
	}
	if err := json.Unmarshal(b, &wrapped); err != nil {
		return fmt.Errorf("parsing wrapped number: %w", err)
	}
	n.Set, n.Number = wrapped.Set, wrapped.Number
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
	TresAlloc   string   `json:"tres_alloc_str"` // totals; carries NO gres on v0.0.44
	TresPerNode string   `json:"tres_per_node"`  // e.g. "gres/gpu:1" (per node)
	GresDetail  []string `json:"gres_detail"`    // e.g. ["gpu:1(IDX:0)"], one per node

	StartTime NoVal `json:"start_time"`
}

type jobsResponse struct {
	Jobs []Job `json:"jobs"`
}

type Client struct {
	base, apiVer, token string
	http                *http.Client
}

func NewClient(base, apiVer, token string) *Client {
	return &Client{base: base, apiVer: apiVer, token: token,
		http: &http.Client{Timeout: 15 * time.Second}}
}

// ListRunningJobs returns jobs currently in the RUNNING state.
func (c *Client) ListRunningJobs(ctx context.Context) ([]Job, error) {
	u := fmt.Sprintf("%s/slurm/%s/jobs", c.base, c.apiVer)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building jobs request: %w", err)
	}
	req.Header.Set("X-SLURM-USER-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling slurmrestd: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("slurmrestd status %s", resp.Status)
	}
	var jr jobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return nil, fmt.Errorf("decoding jobs: %w", err)
	}
	running := make([]Job, 0, len(jr.Jobs))
	for _, j := range jr.Jobs {
		if len(j.State) > 0 && j.State[0] == "RUNNING" {
			running = append(running, j)
		}
	}
	return running, nil
}
