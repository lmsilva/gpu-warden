// Package mecli is squire-me: the answer to "what are my jobs doing", asked
// of a running squire rather than of the cluster.
//
// It is a client and nothing else. squire already holds the joined view -
// slurmrestd, the pod mapping, the telemetry - and serves it as a document,
// so building the view again here would mean a second copy of every
// credential in every user's shell. squire-me holds no credentials at all:
// it asks an HTTP endpoint that answers anyone, and says whose jobs it
// wants. Nothing reachable from here imports Kubernetes or Prometheus, so
// neither is linked - this runs on a login node, next to the person asking.
//
// Which is why the uid it sends proves nothing. The server filters as a
// convenience, and today anyone can ask for anyone's rows, exactly as they
// can on the dashboard. When that stops being acceptable, authentication
// goes in front of the server, and this client keeps working unchanged -
// the seam is the URL, not this binary.
package mecli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/lmsilva/squire/internal/view"
	"github.com/lmsilva/squire/internal/wire"
)

// Exit codes. Separating "found problems" from "could not ask" matters for
// anything scripting this: a squire that is down and a clean report must not
// look the same. The contract is squire-lint's, so the pair scripts the same
// way.
const (
	exitClean    = 0
	exitFindings = 1
	exitError    = 2
)

// timeout bounds one ask. The server gives a cold cache 30 seconds to build
// a pass before it returns an honest 500; a client that gave up sooner would
// turn that into a mystery on this side of the wire.
const timeout = 45 * time.Second

// client is shared so tests and repeated calls reuse connections. The
// timeout above travels in the request context instead, where it covers the
// whole ask.
var client = &http.Client{}

type config struct {
	url        string
	uid        int
	uidSet     bool
	user       string
	jsonOut    bool
	wide       bool
	dollarRate float64
}

// defaultURL is where a squire --serve answers when nobody says otherwise.
// SQUIRE_URL is the site-wide way to say otherwise once - a profile.d line -
// rather than in every invocation.
func defaultURL() string {
	if v := os.Getenv("SQUIRE_URL"); v != "" {
		return v
	}
	return "http://localhost:9101"
}

// parseConfig builds the configuration for one ask. It registers only what
// this binary uses, so the help text describes asking a squire rather than
// running one.
func parseConfig(name string, args []string) config {
	var c config
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.StringVar(&c.url, "url", defaultURL(),
		"base URL of a running squire --serve (or set SQUIRE_URL)")
	fs.IntVar(&c.uid, "uid", 0, "show this numeric uid's jobs instead of your own")
	fs.StringVar(&c.user, "user", "", "show this user's jobs instead of your own (resolved locally)")
	fs.BoolVar(&c.jsonOut, "json", false, "print the server's JSON document instead of the table")
	fs.BoolVar(&c.wide, "wide", false, "add the evidence columns: LIT, FIRST-WORK, WHY")
	fs.Float64Var(&c.dollarRate, "dollar-rate", 0, "cost per wasted GPU-hour, shown next to the hours")
	fs.Parse(args)
	// Visit, because 0 is root - a uid a flag default cannot stand in for.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "uid" {
			c.uidSet = true
		}
	})
	if fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "unexpected argument %q; squire-me takes flags only\n", fs.Arg(0))
		os.Exit(exitError)
	}
	return c
}

// resolveUID decides whose jobs to ask about: --uid, then --user, then the
// invoking user. Both flags at once is a contradiction to report rather than
// an order to pick.
func resolveUID(c config) (int, error) {
	if c.uidSet && c.user != "" {
		return 0, fmt.Errorf("--uid and --user both name an owner; pass one")
	}
	if c.uidSet {
		if c.uid < 0 {
			return 0, fmt.Errorf("--uid must be non-negative, got %d", c.uid)
		}
		return c.uid, nil
	}
	if c.user != "" {
		// Built without cgo this resolves from /etc/passwd and files only,
		// so a user that exists in LDAP alone will not be found here. --uid
		// always works; the error says so.
		u, err := user.Lookup(c.user)
		if err != nil {
			return 0, fmt.Errorf("resolving --user %q locally: %v (--uid always works)", c.user, err)
		}
		n, err := strconv.Atoi(u.Uid)
		if err != nil {
			return 0, fmt.Errorf("uid %q for %q is not a number: %v", u.Uid, c.user, err)
		}
		return n, nil
	}
	return os.Geteuid(), nil
}

// fetch asks the server for the document, already narrowed to one owner.
// The reply is capped: a pass is kilobytes, and a URL that streams more
// than this is not a squire.
func fetch(ctx context.Context, base string, uid int) ([]byte, error) {
	u := strings.TrimSuffix(base, "/") + "/snapshot.json?uid=" + strconv.Itoa(uid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", base, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking %s: %w", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the reply from %s: %w", base, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("squire at %s said %s: %s",
			base, resp.Status, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// run asks, checks the schema, and renders. Everything a person reads goes
// to stdout; every diagnostic goes to stderr - which is not decoration,
// because --json exists to be piped, and a warning mixed into the document
// would corrupt it.
func run(c config, uid int, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body, err := fetch(ctx, c.url, uid)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return exitError
	}
	var s wire.Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		fmt.Fprintf(stderr, "error: %s did not return a squire document: %v\n", c.url, err)
		return exitError
	}
	// Checked in both modes, before a byte reaches stdout. A schema this
	// client does not read would otherwise come out as a table quietly
	// missing fields, or as JSON a script parses under the wrong contract.
	if s.Schema != wire.Schema {
		fmt.Fprintf(stderr,
			"error: the server speaks schema %d and this squire-me reads %d; match their versions\n",
			s.Schema, wire.Schema)
		return exitError
	}

	if c.jsonOut {
		// The server's bytes, not a re-encoding - what jq sees is what the
		// server said - plus the newline a shell expects.
		stdout.Write(body)
		fmt.Fprintln(stdout)
	} else {
		view.Table(stdout, s, view.TableOptions{Wide: c.wide, DollarRate: c.dollarRate})
	}
	if len(s.Findings) > 0 {
		return exitFindings
	}
	return exitClean
}

// Main runs squire-me and returns its exit code: 0 clean, 1 findings about
// the jobs shown, 2 could not ask.
func Main(name string, args []string) int {
	c := parseConfig(name, args)
	uid, err := resolveUID(c)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return exitError
	}
	return run(c, uid, os.Stdout, os.Stderr)
}
