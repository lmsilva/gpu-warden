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

// revision returns the commit the toolchain recorded, and whether the tree
// was dirty when it was built.
//
// Go embeds this automatically since 1.18, but only when it can see a
// repository. A container build copies source without .git, so this is empty
// there - which is exactly why Version is stamped separately rather than
// derived from it.
func revision() (string, bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev, dirty
}

// String renders one line naming the binary, its version, the commit where
// one was recorded, and the toolchain. Everything a bug report needs and
// nothing that has to be looked up.
func String(name string) string {
	s := fmt.Sprintf("%s %s", name, Version)
	if rev, dirty := revision(); rev != "" {
		s += " (" + rev
		if dirty {
			s += ", dirty"
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
