// Package setup installs the things that make muxscale start with the machine
// and pick up agents automatically: a user-level service and PATH shims.
//
// Everything here modifies the user's environment, so everything here follows
// the same three rules: show the exact change before making it, write a backup,
// and provide an uninstall that removes precisely what was added.
package setup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Service describes an installed background service.
type Service struct {
	Kind      string // "launchd" | "systemd"
	UnitPath  string
	Installed bool
	Running   bool
}

// ServicePlan is a pending service installation.
type ServicePlan struct {
	Kind     string
	Path     string
	Contents string
	Exists   bool
	Previous string
	// PostInstall are the commands run to load the service, shown to the user
	// so nothing happens that they did not see.
	PostInstall [][]string
}

const launchdLabel = "com.muxscale.daemon"

// BuildServicePlan computes the service installation for this platform.
//
// User-level only, always. A system-level daemon would run as root, need sudo
// to install, and outlive the user's session -- none of which a personal agent
// monitor has any business doing.
func BuildServicePlan(binaryPath string, tailnet bool) (*ServicePlan, error) {
	if binaryPath == "" {
		var err error
		if binaryPath, err = os.Executable(); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(binaryPath)
	if err != nil {
		return nil, err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	args := []string{"daemon"}
	if tailnet {
		args = append(args, "--tailnet")
	}

	path := ServicePath(home, os.Getenv("PATH"))
	switch runtime.GOOS {
	case "darwin":
		return buildLaunchd(home, abs, args, path)
	case "linux":
		return buildSystemd(home, abs, args, path)
	default:
		return nil, fmt.Errorf("no service integration for %s yet; run `muxscale daemon` manually", runtime.GOOS)
	}
}

// ServicePath is the PATH the service runs with.
//
// A login service does not get the user's shell PATH: launchd gives it a
// bare system one, systemd much the same. The daemon finds the agents by
// running them (`claude --version` and the like), and Claude Code's own
// installer, Codex's and omp's all put them in ~/.local/bin -- so under the
// old fixed PATH a daemon started by `muxscale up` found none of them, set up
// no hooks and no extension, and quietly fell back to guessing, while
// `muxscale status`, run from a shell, said all was well.
//
// So the PATH of the shell that ran `muxscale up` is kept, then the usual
// per-user install directories in case that shell was a sparse one, then the
// system directories. Each once, in that order, and only absolute entries.
func ServicePath(home, shellPath string) string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" || !filepath.IsAbs(dir) || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, d := range filepath.SplitList(shellPath) {
		add(d)
	}
	for _, d := range []string{".local/bin", ".bun/bin", ".opencode/bin", ".npm-global/bin", ".cargo/bin", "go/bin", "bin"} {
		add(filepath.Join(home, d))
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		add(d)
	}
	return strings.Join(out, string(filepath.ListSeparator))
}

func buildLaunchd(home, bin string, args []string, pathEnv string) (*ServicePlan, error) {
	path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	logDir := filepath.Join(home, "Library", "Logs", "muxscale")

	var argXML strings.Builder
	argXML.WriteString("\t\t<string>" + xmlEscape(bin) + "</string>\n")
	for _, a := range args {
		argXML.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}

	contents := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>StandardOutPath</key>
	<string>%s/daemon.log</string>
	<key>StandardErrorPath</key>
	<string>%s/daemon.log</string>
	<key>ProcessType</key>
	<string>Background</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
</dict>
</plist>
`, launchdLabel, argXML.String(), xmlEscape(logDir), xmlEscape(logDir), xmlEscape(pathEnv))

	plan := &ServicePlan{Kind: "launchd", Path: path, Contents: contents}
	if prev, err := os.ReadFile(path); err == nil {
		plan.Exists, plan.Previous = true, string(prev)
	}
	plan.PostInstall = [][]string{
		{"launchctl", "unload", "-w", path},
		{"launchctl", "load", "-w", path},
	}
	return plan, nil
}

func buildSystemd(home, bin string, args []string, pathEnv string) (*ServicePlan, error) {
	path := filepath.Join(home, ".config", "systemd", "user", "muxscaled.service")

	contents := fmt.Sprintf(`[Unit]
Description=muxscale agent monitor
Documentation=https://muxscale.com/docs
After=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Environment="PATH=%s"
Restart=on-failure
RestartSec=5
# The daemon holds PTYs for wrapped agent sessions. Killing only the main
# process on stop leaves those children to be reaped by init, which is better
# than killing an agent mid-tool-call.
KillMode=process
TimeoutStopSec=10

[Install]
WantedBy=default.target
`, bin, strings.Join(args, " "), systemdQuote(pathEnv))

	plan := &ServicePlan{Kind: "systemd", Path: path, Contents: contents}
	if prev, err := os.ReadFile(path); err == nil {
		plan.Exists, plan.Previous = true, string(prev)
	}
	plan.PostInstall = [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", "muxscaled.service"},
	}
	return plan, nil
}

// systemdQuote makes a value safe inside Environment="...": quotes and
// backslashes escaped, and % doubled so systemd does not read it as a
// specifier.
func systemdQuote(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(v)
}

// Apply writes the unit and runs the load commands.
func (p *ServicePlan) Apply() error {
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		return err
	}
	// macOS: the log directory must exist or launchd silently fails to start.
	if p.Kind == "launchd" {
		home, _ := os.UserHomeDir()
		_ = os.MkdirAll(filepath.Join(home, "Library", "Logs", "muxscale"), 0o755)
	}
	if err := os.WriteFile(p.Path, []byte(p.Contents), 0o644); err != nil {
		return err
	}
	for _, cmd := range p.PostInstall {
		// The unload/daemon-reload steps fail harmlessly on a first install,
		// so only the final activating command's failure is fatal.
		_ = exec.Command(cmd[0], cmd[1:]...).Run()
	}
	last := p.PostInstall[len(p.PostInstall)-1]
	if out, err := exec.Command(last[0], last[1:]...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(last, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// UninstallService removes the service.
func UninstallService() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		path := filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return "", fmt.Errorf("no launchd service installed")
		}
		_ = exec.Command("launchctl", "unload", "-w", path).Run()
		return path, os.Remove(path)
	case "linux":
		path := filepath.Join(home, ".config", "systemd", "user", "muxscaled.service")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return "", fmt.Errorf("no systemd unit installed")
		}
		_ = exec.Command("systemctl", "--user", "disable", "--now", "muxscaled.service").Run()
		if err := os.Remove(path); err != nil {
			return "", err
		}
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
		return path, nil
	}
	return "", fmt.Errorf("unsupported platform %s", runtime.GOOS)
}

// ServiceStatus reports whether the service is installed and running.
func ServiceStatus() (Service, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Service{}, err
	}
	switch runtime.GOOS {
	case "darwin":
		s := Service{Kind: "launchd",
			UnitPath: filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")}
		if _, err := os.Stat(s.UnitPath); err == nil {
			s.Installed = true
		}
		out, _ := exec.Command("launchctl", "list").Output()
		s.Running = strings.Contains(string(out), launchdLabel)
		return s, nil
	case "linux":
		s := Service{Kind: "systemd",
			UnitPath: filepath.Join(home, ".config", "systemd", "user", "muxscaled.service")}
		if _, err := os.Stat(s.UnitPath); err == nil {
			s.Installed = true
		}
		out, _ := exec.Command("systemctl", "--user", "is-active", "muxscaled.service").Output()
		s.Running = strings.TrimSpace(string(out)) == "active"
		return s, nil
	}
	return Service{}, fmt.Errorf("unsupported platform %s", runtime.GOOS)
}

// RestartService restarts a running service. launchd needs nothing here:
// Apply already unloads and reloads it, which restarts the daemon.
func RestartService() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if out, err := exec.Command("systemctl", "--user", "restart", "muxscaled.service").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LingerHint explains how to make a systemd user service survive logout.
// Worth surfacing explicitly: without it, the daemon dies when the user logs
// out of an SSH session, which looks like muxscale randomly stopping.
func LingerHint() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	user := os.Getenv("USER")
	if user == "" {
		user = "$USER"
	}
	return fmt.Sprintf(
		"To keep the daemon running after you log out:\n    sudo loginctl enable-linger %s", user)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
