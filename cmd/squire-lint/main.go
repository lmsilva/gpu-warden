// Command squire-lint checks Slurm job configuration and prints what it finds.
//
// It is its own binary rather than a subcommand of squire, because squire
// imports the Kubernetes client and this needs none of it. Nothing reachable
// from here imports Kubernetes or Prometheus, so neither is linked - which is
// the point: this runs on a login node, next to the people submitting the
// jobs, and needs only a slurmrestd URL and a token.
package main

import (
	"fmt"
	"os"

	"github.com/lmsilva/squire/internal/buildinfo"
	"github.com/lmsilva/squire/internal/lintcli"
)

func main() {
	if buildinfo.Asked(os.Args[1:]) {
		fmt.Println(buildinfo.String("squire-lint"))
		return
	}
	os.Exit(lintcli.Main("squire-lint", os.Args[1:]))
}
