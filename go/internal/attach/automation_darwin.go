package attach

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Whether macOS lets this process control iTerm2, asked without sending iTerm2
// anything.
//
// The first Apple event a process sends another app raises macOS's
// Automation prompt, and the event waits on the answer. Sent from the daemon
// -- a background service nobody is watching -- the prompt sat unanswered for
// hours with the event parked behind it, and iTerm2 had to be quit to recover.
// So the daemon never sends iTerm2 an event macOS has not already allowed:
// AEDeterminePermissionToAutomateTarget answers from the consent database,
// without a prompt and without involving iTerm2, and every script is gated on
// it (runOsascript). The prompt is raised only on request, from `muxscale up`,
// with someone at the screen (RequestITerm2Automation).
//
// Called through purego rather than cgo, so the release builds stay
// CGO_ENABLED=0 and cross-compile as before.

const iTerm2BundleID = "com.googlecode.iterm2"

// OSStatus values AEDeterminePermissionToAutomateTarget returns.
const (
	osNoErr                 = 0
	errAEEventNotPermitted  = -1743
	errAEEventWouldRequire  = -1744 // errAEEventWouldRequireUserConsent
	errProcNotFound         = -600
	typeApplicationBundleID = 0x62756e64 // 'bund'
	typeWildCard            = 0x2a2a2a2a // '****'
)

// aeDesc is the C AEDesc: a DescType and a data handle.
type aeDesc struct {
	descriptorType uint32
	dataHandle     uintptr
}

var aeFuncs struct {
	once    sync.Once
	err     error
	create  func(descType uint32, data unsafe.Pointer, size int, result *aeDesc) int16
	perm    func(target *aeDesc, class, id uint32, ask bool) int32
	dispose func(desc *aeDesc) int16
}

func loadAE() error {
	aeFuncs.once.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/CoreServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			aeFuncs.err = fmt.Errorf("loading CoreServices: %w", err)
			return
		}
		purego.RegisterLibFunc(&aeFuncs.create, lib, "AECreateDesc")
		purego.RegisterLibFunc(&aeFuncs.perm, lib, "AEDeterminePermissionToAutomateTarget")
		purego.RegisterLibFunc(&aeFuncs.dispose, lib, "AEDisposeDesc")
	})
	return aeFuncs.err
}

// iTerm2Automation asks macOS whether this process may control iTerm2. With
// ask, it raises the prompt if the user has not decided yet, and blocks until
// they do; without, it never prompts.
func iTerm2Automation(ask bool) (Automation, error) {
	if err := loadAE(); err != nil {
		return AutomationUnknown, err
	}
	id := []byte(iTerm2BundleID)
	var target aeDesc
	if rc := aeFuncs.create(typeApplicationBundleID, unsafe.Pointer(&id[0]), len(id), &target); rc != osNoErr {
		return AutomationUnknown, fmt.Errorf("AECreateDesc: %d", rc)
	}
	defer aeFuncs.dispose(&target)
	switch rc := aeFuncs.perm(&target, typeWildCard, typeWildCard, ask); rc {
	case osNoErr:
		return AutomationAllowed, nil
	case errAEEventWouldRequire:
		return AutomationNotAsked, nil
	case errAEEventNotPermitted:
		return AutomationDenied, nil
	case errProcNotFound:
		return AutomationNotRunning, nil
	default:
		return AutomationUnknown, fmt.Errorf("AEDeterminePermissionToAutomateTarget: %d", rc)
	}
}
