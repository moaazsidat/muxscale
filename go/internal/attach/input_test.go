package attach

import "testing"

func TestArrowsFollowApplicationCursorMode(t *testing.T) {
	for _, tc := range []struct {
		k    NamedKey
		app  bool
		want string
	}{
		{KeyUp, false, "\x1b[A"},
		{KeyUp, true, "\x1bOA"},
		{KeyLeft, true, "\x1bOD"},
		{KeyHome, true, "\x1bOH"},
		{KeyTab, true, "\t"},
	} {
		got, err := EncodeKeyMode(tc.k, tc.app)
		if err != nil || string(got) != tc.want {
			t.Errorf("EncodeKeyMode(%s, %v) = %q, %v; want %q", tc.k, tc.app, got, err, tc.want)
		}
	}
}
