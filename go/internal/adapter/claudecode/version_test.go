package claudecode

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"2.1.220 (Claude Code)", Version{2, 1, 220}, true},
		{"2.1.196", Version{2, 1, 196}, true},
		{"  1.0.0 \n", Version{1, 0, 0}, true},
		{"10.20.30 (Claude Code)", Version{10, 20, 30}, true},
		{"no version here", Version{}, false},
		{"", Version{}, false},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.ok && err != nil {
			t.Errorf("ParseVersion(%q) unexpected error: %v", c.in, err)
			continue
		}
		if !c.ok {
			if err == nil {
				t.Errorf("ParseVersion(%q) expected error, got %v", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("ParseVersion(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The floor is a safety boundary, not a preference: below 2.1.196 there is no
// prompt_id, so input fencing would degrade to wall-clock heuristics. This test
// is the guard on that decision.
func TestClassifyFloor(t *testing.T) {
	cases := []struct {
		version string
		want    Support
	}{
		{"2.1.195", SupportNone},    // one below the floor
		{"2.1.196", SupportFull},    // exactly the floor
		{"2.1.197", SupportFull},
		{"2.1.220", SupportFull},    // newest known profile
		{"2.1.221", SupportUntested},
		{"3.0.0", SupportUntested},
		{"2.0.999", SupportNone},
		{"1.9.9", SupportNone},
	}
	for _, c := range cases {
		v := MustParseVersion(c.version)
		if got := Classify(v); got != c.want {
			t.Errorf("Classify(%s) = %v, want %v", c.version, got, c.want)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.196", "2.1.196", 0},
		{"2.1.195", "2.1.196", -1},
		{"2.1.197", "2.1.196", 1},
		{"2.2.0", "2.1.999", 1},
		{"3.0.0", "2.99.99", 1},
		{"1.0.0", "2.0.0", -1},
	}
	for _, c := range cases {
		got := MustParseVersion(c.a).Compare(MustParseVersion(c.b))
		if got != c.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// An unexplained degradation is worse than a missing feature, so every support
// level must produce actionable text.
func TestSupportMessageIsActionable(t *testing.T) {
	for _, s := range []Support{SupportNone, SupportFull, SupportUntested} {
		msg := SupportMessage(MustParseVersion("2.1.100"), s)
		if msg == "" {
			t.Errorf("SupportMessage for %v is empty", s)
		}
	}
	if got := SupportMessage(MustParseVersion("2.1.100"), SupportNone); !contains(got, MinVersion) {
		t.Errorf("unsupported message must name the floor, got: %s", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
