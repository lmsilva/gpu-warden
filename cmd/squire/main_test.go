package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScrapeBudget(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		// No header: a curl, a health check, anything not Prometheus.
		{"absent", "", cycleTimeout},
		// The shape Prometheus actually sends.
		{"prometheus default", "10.000000", 10*time.Second - scrapeTimeoutOffset},
		// An operator who raised scrape_timeout for this job gets what they set.
		{"raised", "60", 60*time.Second - scrapeTimeoutOffset},
		// Garbage is not a reason to run unbounded.
		{"unparseable", "soon", cycleTimeout},
		// Shorter than the offset: use it whole rather than going negative.
		{"tighter than the offset", "0.2", 200 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if tc.header != "" {
				r.Header.Set("X-Prometheus-Scrape-Timeout-Seconds", tc.header)
			}
			if got := scrapeBudget(r); got != tc.want {
				t.Errorf("header %q: want %v, got %v", tc.header, tc.want, got)
			}
		})
	}
}
