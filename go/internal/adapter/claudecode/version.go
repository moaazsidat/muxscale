package claudecode

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Support describes what the adapter can do against a detected Claude Code.
type Support int

const (
	// SupportNone: version below the floor. We refuse to install hooks and fall
	// back to read-only transcript observation.
	SupportNone Support = iota
	// SupportFull: >= MinVersion. Hooks, structured approvals, fencing.
	SupportFull
	// SupportUntested: newer than any profile we know. Use the newest profile
	// and badge the session -- never fail closed on a version we haven't seen.
	SupportUntested
)

func (s Support) String() string {
	switch s {
	case SupportFull:
		return "full"
	case SupportUntested:
		return "untested-version"
	default:
		return "unsupported"
	}
}

// newestKnownProfile is the highest Claude Code version this build was tested
// against. Bumped when fixtures/hooks/claude-code/<ver>/ is added.
const newestKnownProfile = "2.1.220"

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Version is a parsed semantic version.
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

// ParseVersion extracts a version from arbitrary output such as
// "2.1.220 (Claude Code)".
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

// MustParseVersion is for constants known to be well-formed.
func MustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(err)
	}
	return v
}

// Classify maps a detected version to a support level.
func Classify(v Version) Support {
	if v.Compare(MustParseVersion(MinVersion)) < 0 {
		return SupportNone
	}
	if v.Compare(MustParseVersion(newestKnownProfile)) > 0 {
		return SupportUntested
	}
	return SupportFull
}

// DetectVersion runs `claude --version`.
func DetectVersion() (Version, error) {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return Version{}, fmt.Errorf("running claude --version: %w", err)
	}
	return ParseVersion(strings.TrimSpace(string(out)))
}

// SupportMessage explains a support level to a human, including what to do
// about it. An unexplained degradation is worse than a missing feature.
func SupportMessage(v Version, s Support) string {
	switch s {
	case SupportNone:
		return fmt.Sprintf(
			"Claude Code %s is below the supported floor of %s.\n"+
				"Sessions will still appear (read-only, state inferred from the transcript),\n"+
				"but hooks, remote approvals, and reliable state need an upgrade.",
			v, MinVersion)
	case SupportUntested:
		return fmt.Sprintf(
			"Claude Code %s is newer than the newest profile muxscale was tested against (%s).\n"+
				"Using that profile; sessions are badged untested-version.",
			v, newestKnownProfile)
	default:
		return fmt.Sprintf("Claude Code %s: fully supported.", v)
	}
}
