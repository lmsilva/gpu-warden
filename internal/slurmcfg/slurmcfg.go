// Package slurmcfg holds the settings needed to reach slurmrestd, and binds
// them to a command-line flag set.
//
// It exists so the two entry points that talk to Slurm - the monitoring binary
// and the configuration check - register the same flags with the same defaults
// from one definition. Duplicating two StringVar calls would work until the day
// one default changed and the other did not.
package slurmcfg

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Config is everything needed to reach slurmrestd, and nothing else.
type Config struct {
	URL     string
	Version string

	// TokenFile, when set, is read on every request rather than once at
	// startup. That is what lets a rotated secret reach a running server: a
	// process cannot have its environment changed from outside, so a token
	// taken from SLURM_JWT is fixed for the life of the process.
	TokenFile string

	// env holds SLURM_JWT as it stood when the flags were bound. Unexported
	// so a caller reaches the token through Token(), which is the only place
	// that knows a file may take precedence.
	env string
}

// Token returns the token to send with the next request. Safe to pass as a
// method value wherever a token source is wanted.
//
// It fails with a sentence rather than sending an empty header, because an
// empty token produces a 511 from slurmrestd that reads like a permissions
// problem rather than a missing credential.
func (c Config) Token() (string, error) {
	if c.TokenFile == "" {
		if c.env == "" {
			return "", errors.New("no Slurm token: set SLURM_JWT, or point --slurm-token-file at a mounted one")
		}
		return c.env, nil
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", c.TokenFile, err)
	}
	// Trailing newlines are what every tool that writes a token adds, and an
	// HTTP header carrying one is rejected.
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", c.TokenFile)
	}
	return tok, nil
}

// envOr returns the environment value for key, or def when it is unset.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Bind registers the connection flags on fs and reads the token.
//
// The token comes from the environment only, never a flag: a flag would put a
// credential in every ps listing on the machine.
func Bind(fs *flag.FlagSet, c *Config) {
	fs.StringVar(&c.URL, "slurm-url", envOr("SQUIRE_SLURM_URL", "http://localhost:6820"), "slurmrestd base URL")
	fs.StringVar(&c.Version, "slurm-api", envOr("SQUIRE_SLURM_API", "v0.0.44"), "slurmrestd API version")
	fs.StringVar(&c.TokenFile, "slurm-token-file", os.Getenv("SQUIRE_SLURM_TOKEN_FILE"),
		"read the Slurm token from this file on every request instead of from SLURM_JWT")
	c.env = os.Getenv("SLURM_JWT")
}
