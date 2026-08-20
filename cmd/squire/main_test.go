package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

// TestPageRefresh pins the cadence the page reloads itself on. It follows the
// cache so a reload costs nothing extra, and never drops below the floor -
// with the cache off, following it exactly would mean a full fan-out per
// reload per browser.
func TestPageRefresh(t *testing.T) {
	cases := []struct {
		name  string
		cache time.Duration
		want  time.Duration
	}{
		{"default cache", 30 * time.Second, 30 * time.Second},
		{"long cache", 5 * time.Minute, 5 * time.Minute},
		{"short cache", 5 * time.Second, minPageRefresh},
		{"cache disabled", 0, minPageRefresh},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageRefresh(tc.cache); got != tc.want {
				t.Errorf("cache %v: want %v, got %v", tc.cache, tc.want, got)
			}
		})
	}
}

// TestIgnoredFlagNote pins both directions. A flag that is silently ignored
// is worse than one that is rejected: the run succeeds, the output looks
// right, and the setting the operator asked for is simply absent.
func TestIgnoredFlagNote(t *testing.T) {
	cases := []struct {
		name          string
		serving       bool
		serveCacheSet bool
		watch         time.Duration
		want          string
	}{
		{"serving, nothing ignored", true, true, 0, ""},
		{"serving with a watch interval", true, false, 30 * time.Second,
			"note: --watch is ignored with --serve; Prometheus sets the cadence"},
		{"one-shot with a cache setting", false, true, 0,
			"note: --serve-cache is ignored without --serve; a one-shot or --watch run builds every cycle"},
		{"watch mode with a cache setting", false, true, 30 * time.Second,
			"note: --serve-cache is ignored without --serve; a one-shot or --watch run builds every cycle"},
		// The default is not a request. Only a flag actually named counts,
		// which is why the value alone cannot answer this.
		{"one-shot, cache left alone", false, false, 0, ""},
		{"watch mode, cache left alone", false, false, 30 * time.Second, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ignoredFlagNote(tc.serving, tc.serveCacheSet, tc.watch); got != tc.want {
				t.Errorf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestServeCacheSetOnlyWhenNamed covers the parse half: a --serve-cache equal
// to the default is still a request, and no flag at all is not.
func TestServeCacheSetOnlyWhenNamed(t *testing.T) {
	if parseConfig([]string{}).serveCacheSet {
		t.Error("an unnamed flag must not read as set")
	}
	if !parseConfig([]string{"--serve-cache", "30s"}).serveCacheSet {
		t.Error("naming the flag at its default value is still naming it")
	}
	if !parseConfig([]string{"--serve-cache", "0"}).serveCacheSet {
		t.Error("a zero value is a legitimate setting and must read as set")
	}
}

// TestParseUID covers the one route parameter. Absent means the whole
// cluster; 0 is root, an owner rather than an absence; and names are
// rejected because the machine field is numeric - the display name is for
// people.
func TestParseUID(t *testing.T) {
	cases := []struct {
		query   string
		uid     int
		filter  bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"uid=50000", 50000, true, false},
		{"uid=0", 0, true, false},
		{"uid=", 0, false, false}, // present but empty reads as absent
		{"uid=-1", 0, false, true},
		{"uid=ana", 0, false, true},
		{"uid=1.5", 0, false, true},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("bad case %q: %v", c.query, err)
		}
		uid, filter, err := parseUID(q)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v, want error %v", c.query, err, c.wantErr)
			continue
		}
		if uid != c.uid || filter != c.filter {
			t.Errorf("%q: got (%d, %v), want (%d, %v)", c.query, uid, filter, c.uid, c.filter)
		}
	}
}

// TestParseFindingFilter: two optional terms, and a job number that must be a
// real one. A rule name is passed through unchecked - the server cannot
// validate it without pinning every rule name into the URL contract, and a
// script asking for a rule nobody tripped wants an empty list.
func TestParseFindingFilter(t *testing.T) {
	cases := []struct {
		query   string
		rule    string
		job     int
		wantErr bool
	}{
		{"", "", 0, false},
		{"rule=no-time-limit", "no-time-limit", 0, false},
		{"job=101", "", 101, false},
		{"rule=no-time-limit&job=101", "no-time-limit", 101, false},
		{"rule=no-such-rule", "no-such-rule", 0, false},
		{"job=", "", 0, false},
		{"job=0", "", 0, true},
		{"job=-1", "", 0, true},
		{"job=train", "", 0, true},
	}
	for _, c := range cases {
		q, err := url.ParseQuery(c.query)
		if err != nil {
			t.Fatalf("bad case %q: %v", c.query, err)
		}
		f, err := parseFindingFilter(q)
		if (err != nil) != c.wantErr {
			t.Errorf("%q: err = %v, want error %v", c.query, err, c.wantErr)
			continue
		}
		if err == nil && (f.Rule != c.rule || f.JobID != c.job) {
			t.Errorf("%q: got %+v, want rule %q job %d", c.query, f, c.rule, c.job)
		}
	}
}
