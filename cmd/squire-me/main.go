// Command squire-me shows a person their own GPU jobs, by asking a running
// squire.
//
// It is its own binary rather than a subcommand of squire, for squire-lint's
// reason taken one step further: this needs no Kubernetes, no Prometheus,
// and no slurmrestd token either - only the URL of a squire that has all
// three. It runs on a login node with no credentials at all, which is the
// design: what a user's shell cannot hold, a user's shell cannot leak.
package main

import (
	"fmt"
	"os"

	"github.com/lmsilva/squire/internal/buildinfo"
	"github.com/lmsilva/squire/internal/mecli"
)

func main() {
	if buildinfo.Asked(os.Args[1:]) {
		fmt.Println(buildinfo.String("squire-me"))
		return
	}
	os.Exit(mecli.Main("squire-me", os.Args[1:]))
}
