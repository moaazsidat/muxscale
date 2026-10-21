package attach

import (
	"strings"
	"testing"
)

func TestDetectTerminalsExplainsItself(t *testing.T) {
	caps := DetectTerminals()
	if len(caps) != 3 {
		t.Fatalf("expected 3 terminals, got %d", len(caps))
	}
	for _, c := range caps {
		// Every unavailable or disabled terminal must say why, and an
		// actionable fix is what turns "not supported" into "one config line".
		if !c.Enabled && c.Reason == "" {
			t.Errorf("%s is not enabled but gives no reason", c.Kind)
		}
		if c.Available && !c.Enabled && c.HowToFix == "" {
			t.Errorf("%s is installed but disabled with no fix suggested", c.Kind)
		}
		t.Logf("%-8s available=%v enabled=%v  %s", c.Kind, c.Available, c.Enabled, c.Reason)
	}
}

func TestParseKittyLS(t *testing.T) {
	// Shape taken from kitty's documented `@ ls` output.
	sample := `[
	  {"tabs": [
	    {"windows": [
	      {"id": 1, "title": "claude", "cwd": "/home/dev/app",
	       "foreground_processes": [{"cmdline": ["/usr/local/bin/claude"], "cwd": "/home/dev/app"}]},
	      {"id": 2, "title": "zsh", "cwd": "/home/dev",
	       "foreground_processes": [{"cmdline": ["/bin/zsh"], "cwd": "/home/dev"}]}
	    ]}
	  ]}
	]`
	panes, err := parseKittyLS([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2", len(panes))
	}
	if panes[0].ID != 1 || panes[0].Command != "claude" || panes[0].Cwd != "/home/dev/app" {
		t.Errorf("pane 0 = %+v", panes[0])
	}
	if panes[1].Command != "zsh" {
		t.Errorf("pane 1 command = %q", panes[1].Command)
	}
}

func TestParseKittyLSFallsBackToProcessCwd(t *testing.T) {
	sample := `[{"tabs":[{"windows":[
	  {"id": 7, "title": "x", "cwd": "",
	   "foreground_processes": [{"cmdline": ["/usr/bin/codex"], "cwd": "/srv/project"}]}
	]}]}]`
	panes, err := parseKittyLS([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if panes[0].Cwd != "/srv/project" {
		t.Errorf("cwd = %q, want the foreground process's cwd", panes[0].Cwd)
	}
}

// Sampled sessions must be labelled. Presenting a 2 Hz poll as a live terminal
// is a lie the UI cannot recover from.
func TestSampledNoticeIsHonest(t *testing.T) {
	if !strings.Contains(SampledNotice, "not a live stream") {
		t.Errorf("SampledNotice should say plainly that it is not live: %q", SampledNotice)
	}
}

func TestParseListenOn(t *testing.T) {
	conf := "# listen_on none\nlisten_on unix:/tmp/old\nallow_remote_control yes\nlisten_on unix:/tmp/kitty\n"
	if got := parseListenOn(conf); got != "unix:/tmp/kitty" {
		t.Errorf("last listen_on should win, got %q", got)
	}
	if got := parseListenOn("# listen_on unix:/tmp/kitty\n"); got != "" {
		t.Errorf("a commented line is not a setting, got %q", got)
	}
}

func TestKittySocketPattern(t *testing.T) {
	// kitty appends -<pid> to a configured path unless it places the pid itself.
	if got := kittySocketPattern("/tmp/kitty"); got != "/tmp/kitty-*" {
		t.Errorf("plain path: %q", got)
	}
	if got := kittySocketPattern("/run/kitty-{kitty_pid}.sock"); got != "/run/kitty-*.sock" {
		t.Errorf("templated path: %q", got)
	}
}

func TestParseKittyLSWindowPid(t *testing.T) {
	sample := `[{"tabs":[{"windows":[{"id": 2, "pid": 417858, "title": "x",
	   "foreground_processes": [{"pid": 425302, "cmdline": ["claude"]}]}]}]}]`
	panes, err := parseKittyLS([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if panes[0].Pid != 417858 {
		t.Errorf("window pid = %d, want the window's own process, not the foreground one", panes[0].Pid)
	}
}
