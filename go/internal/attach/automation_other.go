//go:build !darwin

package attach

// iTerm2 is macOS-only; elsewhere there is nothing to ask and nothing to
// control.
func iTerm2Automation(bool) (Automation, error) { return AutomationNotRunning, nil }
