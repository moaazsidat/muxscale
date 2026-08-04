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

	switch runtime.GOOS {
	case "darwin":
		return buildLaunchd(home, abs, args)
	case "linux":
		return buildSystemd(home, abs, args)
	default:
		return nil, fmt.Errorf("no service integration for %s yet; run `muxscale daemon` manually", runtime.GOOS)
	}
}

func buildLaunchd(home, bin string, args []string) (*ServicePlan, error) {
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
		<string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
</dict>
</plist>
`, launchdLabel, argXML.String(), xmlEscape(logDir), xmlEscape(logDir))

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

func buildSystemd(home, bin string, args []string) (*ServicePlan, error) {
	path := filepath.Join(home, ".config", "systemd", "user", "muxscaled.service")

	contents := fmt.Sprintf(`[Unit]
Description=muxscale agent monitor
Documentation=https://muxscale.com/docs
After=network-online.target

[Service]
Type=simple
ExecStart=%s %s
Restart=on-failure
RestartSec=5
# The daemon holds PTYs for wrapped agent sessions. Killing only the main
# process on stop leaves those children to be reaped by init, which is better
# than killing an agent mid-tool-call.
KillMode=process
TimeoutStopSec=10

[Install]
WantedBy=default.target
`, bin, strings.Join(args, " "))

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
