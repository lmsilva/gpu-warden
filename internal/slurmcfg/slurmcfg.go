// Package slurmcfg holds the settings needed to reach slurmrestd, and binds
// them to a command-line flag set.
//
// It exists so the two entry points that talk to Slurm - the monitoring binary
// and the configuration check - register the same flags with the same defaults
// from one definition. Duplicating two StringVar calls would work until the day
// one default changed and the other did not.
package slurmcfg

import (
	"flag"
	"os"
)

// Config is everything needed to reach slurmrestd, and nothing else.
type Config struct {
	URL     string
	Version string
	Token   string
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
	c.Token = os.Getenv("SLURM_JWT")
}
