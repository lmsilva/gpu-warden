package buildinfo

import (
	"strings"
	"testing"
)

// TestUnstampedSaysDev: a binary nobody released must not claim a release
// number. "dev" in a bug report is itself useful.
func TestUnstampedSaysDev(t *testing.T) {
	if Version != "dev" {
		t.Skip("this build was stamped; nothing to check")
	}
	if !strings.Contains(String("squire"), "squire dev") {
		t.Errorf("an unstamped build must say dev: %s", String("squire"))
	}
}

// TestStringNamesTheBinary: both binaries share this, so the name has to come
// from the caller rather than being baked in.
func TestStringNamesTheBinary(t *testing.T) {
	for _, name := range []string{"squire", "squire-lint"} {
		if !strings.HasPrefix(String(name), name+" ") {
			t.Errorf("String(%q) = %q, want it to lead with the name", name, String(name))
		}
	}
	if !strings.Contains(String("squire"), "go1.") {
		t.Errorf("the toolchain belongs in the line: %s", String("squire"))
	}
}

// TestAsked accepts either spelling and looks anywhere in the arguments,
// because --version has to work even when the rest of the command line does
// not.
func TestAsked(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-version"}, true},
		{[]string{"--serve", ":9101", "--version"}, true},
		{[]string{"--nonsense", "--version"}, true},
		{[]string{"--serve", ":9101"}, false},
		{nil, false},
		// Not a version request: a value that happens to read like one.
		{[]string{"--slurm-api", "version"}, false},
	}
	for _, c := range cases {
		if got := Asked(c.args); got != c.want {
			t.Errorf("Asked(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

// TestTreeStateNeverGuesses: the three states exist because "no uncommitted
// changes" and "no repository to look at" are different facts. A container
// build records nothing, and reporting clean there would be a definite claim
// about something never measured - the same error as printing 0 for a GPU
// count that was not read.
func TestTreeStateNeverGuesses(t *testing.T) {
	rev, state := Revision()
	switch state {
	case TreeClean, TreeModified:
		if rev == "" {
			t.Errorf("state %q claims to know the tree, but no commit was recorded", state)
		}
	case TreeUnrecorded:
		// Fine either way: a partial record reports unrecorded and may
		// still carry the commit it did see.
	default:
		t.Errorf("unknown tree state %q", state)
	}
}

// TestStringMarksOnlyModified: an unrecorded tree adds nothing to the line,
// because there is nothing to warn about - the caveat is for a binary whose
// code is not the commit it names.
func TestStringMarksOnlyModified(t *testing.T) {
	_, state := Revision()
	got := String("squire")
	if state != TreeModified && strings.Contains(got, "modified") {
		t.Errorf("state is %q but the line claims modified: %s", state, got)
	}
}
