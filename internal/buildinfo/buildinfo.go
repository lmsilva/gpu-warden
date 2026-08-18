// Package buildinfo carries the version a binary was built with, and renders
// it for --version.
//
// It exists so both binaries answer the question the same way, and so the
// answer is honest about not knowing: an unstamped build says "dev" rather
// than inventing a number.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is stamped at build time with
// -ldflags "-X github.com/lmsilva/squire/internal/buildinfo.Version=v1.2.3".
//
// It stays "dev" for a plain `go build`, which is the truthful answer: a
// binary nobody released should not claim a release number, and "dev" in a
// bug report is itself useful information.
var Version = "dev"

// TreeState is what the toolchain recorded about the working tree the binary
// was built from.
//
// Three states rather than a boolean, because "not dirty" and "nothing was
// recorded" are different facts and only one of them is reassuring. A
// container build copies source without .git, so the toolchain records
// nothing at all - and a boolean has no way to say so, leaving an unknown
// printed as a definite value.
type TreeState string

const (
	// TreeClean: the toolchain saw a repository and no uncommitted changes.
	TreeClean TreeState = "clean"
	// TreeModified: it saw uncommitted changes, so the recorded commit does
	// not describe this binary.
	TreeModified TreeState = "modified"
	// TreeUnrecorded: there was no repository to look at. Every CI build is
	// this, and so is every container build.
	TreeUnrecorded TreeState = "unrecorded"
)

// revision returns the commit the toolchain recorded and the state of the
// tree it was built from.
//
// Go embeds this automatically since 1.18, but only when it can see a
// repository. A container build copies source without .git, so this reports
// TreeUnrecorded there - which is exactly why Version is stamped separately
// rather than derived from it.
func revision() (string, TreeState) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", TreeUnrecorded
	}
	var rev string
	var modified, sawModified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified, sawModified = s.Value == "true", true
		}
	}
	// Both settings arrive together or not at all. Requiring both rather
	// than either means a partial record reports unrecorded instead of
	// guessing at the half that is missing.
	if rev == "" || !sawModified {
		return rev, TreeUnrecorded
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified {
		return rev, TreeModified
	}
	return rev, TreeClean
}

// Revision exposes what the toolchain recorded, for callers that need the
// parts rather than the rendered line. The commit is empty when nothing was
// recorded, which is the container case.
func Revision() (commit string, state TreeState) { return revision() }

// String renders one line naming the binary, its version, the commit where
// one was recorded, and the toolchain. Everything a bug report needs and
// nothing that has to be looked up.
func String(name string) string {
	s := fmt.Sprintf("%s %s", name, Version)
	if rev, state := revision(); rev != "" {
		s += " (" + rev
		if state == TreeModified {
			s += ", modified"
		}
		s += ")"
	}
	return s + fmt.Sprintf(" %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// Asked reports whether args request the version, and is deliberately
// checked before any flag parsing.
//
// --version is answered the same way whatever else is on the command line,
// and a binary that cannot start because a flag is wrong should still be able
// to say what it is.
func Asked(args []string) bool {
	for _, a := range args {
		if a == "--version" || a == "-version" {
			return true
		}
	}
	return false
}
