package adapter

import (
	"fmt"
	"regexp"
	"strconv"
)

// Version comparison, shared by every adapter.
//
// This began inside the Claude Code adapter and moved here unchanged the moment
// there was a second adapter with a version floor: five copies of semver
// parsing is five places for the comparison to be subtly wrong, and the one
// that matters -- "is this agent new enough for hooks" -- decides whether we
// write to someone's config file.

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Version is a parsed semantic version. Pre-release and build metadata are
// deliberately ignored: agents ship versions like "0.130.0-alpha.3" and
// "2.1.220 (Claude Code)", and for the only question asked here -- is this at
// or above a floor -- the numeric triple is what matters.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare returns -1, 0, or 1.
func (v Version) Compare(o Version) int {
	for _, p := range [][2]int{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// ParseVersion extracts a version from arbitrary command output, such as
// "2.1.220 (Claude Code)" or "pi version 0.80.1".
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("no version found in %q", s)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return Version{maj, min, pat}, nil
}

// MustParseVersion is for constants known to be well-formed. It panics, which
// is correct: a malformed MinVersion is a build-time mistake, and the
// conformance test calls this for every registered adapter so it surfaces then.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// VersionSupport grades a detected agent version against an adapter's floor.
type VersionSupport int

const (
	// VersionTooOld: below MinVersion. Richer features are refused and the
	// session falls back to read-only observation. It still appears.
	VersionTooOld VersionSupport = iota
	// VersionSupported: at or above the floor, at or below the newest tested.
	VersionSupported
	// VersionUntested: newer than any profile in this build. The newest profile
	// is used and the session is badged.
	//
	// This is never an error. Agents auto-update; failing closed on an
	// unrecognised version would break every user simultaneously, on a day
	// muxscale did not ship anything.
	VersionUntested
)

func (s VersionSupport) String() string {
	switch s {
	case VersionSupported:
		return "supported"
	case VersionUntested:
		return "untested-version"
	default:
		return "unsupported"
	}
}

// ClassifyVersion grades v against the floor and ceiling declared in Info.
func (i Info) ClassifyVersion(v Version) VersionSupport {
	if v.Compare(MustParseVersion(i.MinVersion)) < 0 {
		return VersionTooOld
	}
	if i.NewestTested != "" && v.Compare(MustParseVersion(i.NewestTested)) > 0 {
		return VersionUntested
	}
	return VersionSupported
}

// VersionMessage explains a classification to a human, including what to do
// about it. A degradation the user cannot explain is worse than a missing
// feature, because they conclude the tool is broken rather than out of date.
func (i Info) VersionMessage(v Version, s VersionSupport) string {
	switch s {
	case VersionTooOld:
		return fmt.Sprintf(
			"%s %s is below the supported floor of %s.\n"+
				"Sessions will still appear (read-only, state inferred from the transcript),\n"+
				"but hooks, remote approvals, and reliable state need an upgrade.",
			i.DisplayName, v, i.MinVersion)
	case VersionUntested:
		return fmt.Sprintf(
			"%s %s is newer than the newest profile muxscale was tested against (%s).\n"+
				"Using that profile; sessions are badged untested-version.",
			i.DisplayName, v, i.NewestTested)
	default:
		return fmt.Sprintf("%s %s: fully supported.", i.DisplayName, v)
	}
}
