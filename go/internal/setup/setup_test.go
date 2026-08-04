package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildServicePlanForThisPlatform(t *testing.T) {
	plan, err := BuildServicePlan("/usr/local/bin/muxscale", true)
	if err != nil {
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			t.Skipf("no service integration for %s", runtime.GOOS)
		}
		t.Fatal(err)
	}
	if plan.Contents == "" || plan.Path == "" {
		t.Fatal("empty plan")
	}
	if !strings.Contains(plan.Contents, "/usr/local/bin/muxscale") {
		t.Error("plan does not reference the binary path")
	}
	if !strings.Contains(plan.Contents, "--tailnet") {
		t.Error("tailnet flag not propagated into the unit")
	}
	// User-level only: a system path here would mean sudo and running as root.
	home, _ := os.UserHomeDir()
	if !strings.HasPrefix(plan.Path, home) {
		t.Errorf("unit path %q is outside the user's home; services must be user-level", plan.Path)
	}
	t.Logf("%s unit at %s", plan.Kind, plan.Path)
}

func TestServicePlanEscapesPaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plist escaping is macOS-specific")
	}
	plan, err := BuildServicePlan("/tmp/weird & <path>/muxscale", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plan.Contents, "& <path>") {
		t.Error("plist contents were not XML-escaped; the file would be malformed")
	}
	if !strings.Contains(plan.Contents, "&amp;") {
		t.Error("expected an escaped ampersand")
	}
}

func TestShimPlanFindsRealBinary(t *testing.T) {
	// `sh` exists everywhere and is a safe stand-in for an agent binary.
	plan, err := BuildShimPlan([]string{"sh", "definitely-not-installed-xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.Agents["sh"]; !ok {
		t.Error("sh should have resolved")
	}
	if len(plan.Skipped) != 1 || plan.Skipped[0] != "definitely-not-installed-xyz" {
		t.Errorf("skipped = %v, want the missing binary", plan.Skipped)
	}
	if !strings.Contains(plan.PathHint, plan.Dir) {
		t.Error("PathHint should mention the shim dir")
	}
}

// The shim must never resolve to itself, or the wrapper would exec itself
// forever. This is the failure that would look like a hung terminal.
func TestShimNeverResolvesToItself(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "sh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	old := os.Getenv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", old) })
	os.Setenv("PATH", dir+string(os.PathListSeparator)+old)

	real, err := resolveRealBinary("sh", dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(real) == dir {
		t.Errorf("resolved to the shim dir itself: %s", real)
	}
}

func TestShimContentGuards(t *testing.T) {
	dir := t.TempDir()
	plan := &ShimPlan{Dir: dir, Agents: map[string]string{"claude": "/usr/local/bin/claude"}}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "claude"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)

	for _, guard := range []struct{ needle, why string }{
		{"$MUXSCALE_WRAPPED", "must not re-wrap itself into infinite recursion"},
		{"[ ! -t 1 ]", "must stay out of the way of scripts and CI"},
		{`"$MUXSCALE_SHIM" = "0"`, "must offer an explicit opt-out"},
		{"exec ", "must exec, or the shim's shell breaks job control"},
		{shimMarker, "must be identifiable for a precise uninstall"},
	} {
		if !strings.Contains(content, guard.needle) {
			t.Errorf("shim missing %q: it %s", guard.needle, guard.why)
		}
	}

	fi, _ := os.Stat(filepath.Join(dir, "claude"))
	if fi.Mode().Perm()&0o111 == 0 {
		t.Error("shim is not executable")
	}
}

// A shim that ran the real binary but broke on paths with spaces would be an
// awful bug to diagnose, so the path must be quoted.
func TestShimQuotesPathsWithSpaces(t *testing.T) {
	dir := t.TempDir()
	plan := &ShimPlan{Dir: dir, Agents: map[string]string{
		"claude": "/Applications/My Tools/claude",
	}}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "claude"))
	if !strings.Contains(string(b), `REAL="/Applications/My Tools/claude"`) {
		t.Errorf("path with spaces was not quoted:\n%s", b)
	}
}

// Uninstall must remove only muxscale's files. A user's own script that
// happens to share a name stays.
func TestUninstallRemovesOnlyMarkedShims(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".muxscale", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := &ShimPlan{Dir: dir, Agents: map[string]string{"claude": "/bin/echo"}}
	if err := plan.Apply(); err != nil {
		t.Fatal(err)
	}
	// A file muxscale did not write.
	userScript := filepath.Join(dir, "my-own-tool")
	if err := os.WriteFile(userScript, []byte("#!/bin/sh\necho mine\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	removed, err := UninstallShims()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "claude" {
		t.Errorf("removed = %v, want just the muxscale shim", removed)
	}
	if _, err := os.Stat(userScript); err != nil {
		t.Error("uninstall deleted a file muxscale did not create")
	}
}

func TestInstalledShimsIdentifiesByMarker(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".muxscale", "bin")
	_ = os.MkdirAll(dir, 0o755)

	_ = os.WriteFile(filepath.Join(dir, "unmarked"), []byte("#!/bin/sh\n"), 0o755)
	plan := &ShimPlan{Dir: dir, Agents: map[string]string{"codex": "/bin/echo"}}
	_ = plan.Apply()

	got, err := InstalledShims()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "codex" {
		t.Errorf("InstalledShims() = %v, want [codex]", got)
	}
}

func TestLingerHintOnlyOnLinux(t *testing.T) {
	got := LingerHint()
	if runtime.GOOS == "linux" && got == "" {
		t.Error("linux should get a linger hint; without it the daemon dies at logout")
	}
	if runtime.GOOS != "linux" && got != "" {
		t.Errorf("non-linux should not get a systemd hint, got %q", got)
	}
}
