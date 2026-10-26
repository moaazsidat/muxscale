package attach

import "fmt"

// Automation is whether macOS lets this process control iTerm2 (see
// automation_darwin.go).
type Automation string

const (
	AutomationAllowed Automation = "allowed"
	// AutomationNotAsked: nobody has answered the prompt for this process yet.
	AutomationNotAsked Automation = "not_asked"
	// AutomationDenied: refused, in the prompt or in System Settings. macOS
	// does not ask again; only the switch in System Settings changes it.
	AutomationDenied     Automation = "denied"
	AutomationNotRunning Automation = "not_running"
	AutomationUnknown    Automation = "unknown"
)

// ITerm2Automation reports whether this process may control iTerm2, without
// prompting and without sending iTerm2 anything.
func ITerm2Automation() (Automation, error) { return iTerm2Automation(false) }

// RequestITerm2Automation raises macOS's prompt for controlling iTerm2 if the
// user has not answered it yet, and waits for the answer. Only for when
// someone is at the screen and has asked for it: `muxscale up`.
func RequestITerm2Automation() (Automation, error) { return iTerm2Automation(true) }

// ITerm2Installed reports whether iTerm2 is installed where macOS puts apps.
func ITerm2Installed() bool { return iTermInstalled() }

// iTerm2Gate is what runOsascript checks before every script: nil when macOS
// already allows it, else why not. A variable so tests can open it.
var iTerm2Gate = func() error {
	a, err := ITerm2Automation()
	if err != nil {
		return fmt.Errorf("could not ask macOS whether muxscale may control iTerm2: %w", err)
	}
	return automationError(a)
}

func automationError(a Automation) error {
	switch a {
	case AutomationAllowed:
		return nil
	case AutomationNotRunning:
		return fmt.Errorf("iTerm2 is not running")
	case AutomationNotAsked:
		return fmt.Errorf("muxscale has not been allowed to control iTerm2 yet.\n" +
			"Run `muxscale up` and click OK when macOS asks")
	case AutomationDenied:
		return fmt.Errorf("macOS has not allowed muxscale to control iTerm2.\n" +
			"Turn it on in System Settings > Privacy & Security > Automation, " +
			"under muxscale (or the terminal the daemon was started from)")
	}
	return fmt.Errorf("could not tell whether muxscale may control iTerm2")
}
