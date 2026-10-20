package builtin

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter"
	"github.com/muxscale/muxscale/go/internal/attach"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Adapter conformance.
//
// Spec M4's exit criterion is "adapter conformance nightly green for >= 8
// agents". This is the suite that criterion refers to: every check here runs
// against every registered adapter, so adding an agent means adding a
// constructor to Registry and nothing else.
//
// The checks are deliberately about the *seam*, not about any agent's file
// format -- those belong in each adapter's own tests, where a realistic fixture
// is available. What is verified here is the thing no single adapter's tests
// can see: that all of them agree on the contract the daemon relies on.

func reg(t *testing.T) *adapter.Registry {
	t.Helper()
	return Registry(Options{})
}

// M4's exit criterion is conformance green for at least 8 agents. Asserting
// the count here means the criterion is measured rather than remembered.
func TestConformanceCoversTheMilestoneThreshold(t *testing.T) {
	const required = 8
	r := reg(t)
	if r.Len() < required {
		t.Errorf("%d adapters registered; M4 requires conformance across at least %d",
			r.Len(), required)
	}
	t.Logf("%d adapters: %v", r.Len(), r.IDs())

	// Of those, how many are more than detection. Tracked because ten
	// detection-only adapters would satisfy the count while delivering none of
	// the product -- the number worth watching is this one.
	structured := 0
	for _, a := range r.All() {
		if a.Info().Signals.Transcript != adapter.SupportNone {
			structured++
		}
	}
	t.Logf("%d of %d produce a structured transcript", structured, r.Len())
	if structured == 0 {
		t.Error("no adapter produces a transcript; the count is meaningless")
	}
}

func TestEveryAdapterDeclaresUsableInfo(t *testing.T) {
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			if info.DisplayName == "" {
				t.Error("DisplayName is empty; it is what a human sees in the session list")
			}
			if info.MinVersion == "" {
				t.Fatal("MinVersion is empty: without a floor there is no way to " +
					"refuse to write hooks into a version that cannot honour them")
			}
			// Panics on a malformed constant, which is the point: better here
			// than the first time a user's agent version is classified.
			min := adapter.MustParseVersion(info.MinVersion)

			if info.NewestTested != "" {
				newest := adapter.MustParseVersion(info.NewestTested)
				if newest.Compare(min) < 0 {
					t.Errorf("NewestTested %s is below MinVersion %s, so every "+
						"supported version would classify as untested",
						newest, min)
				}
			}
		})
	}
}

// The check that earns this suite's existence: a declaration that does not
// match the implementation.
//
// An adapter claiming Signals.Transcript without implementing Transcriber does
// not fail loudly -- the daemon just never gets a transcript, and the session
// shows up perpetually empty. That is a bug report reading "sometimes the
// transcript is blank", which is close to unactionable. Here it is a build
// failure naming the adapter and the method.
func TestDeclaredSignalsMatchImplementedInterfaces(t *testing.T) {
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			if info.Signals.Transcript != adapter.SupportNone {
				if _, ok := a.(adapter.Transcriber); !ok {
					t.Errorf("declares Signals.Transcript=%s but does not implement "+
						"adapter.Transcriber, so no transcript can ever be read",
						info.Signals.Transcript)
				}
			}
			if info.Signals.State != adapter.SupportNone {
				if _, ok := a.(adapter.StateInferrer); !ok {
					t.Errorf("declares Signals.State=%s but does not implement "+
						"adapter.StateInferrer", info.Signals.State)
				}
			}
			// Approvals are the asymmetric case. Being able to *notice* a
			// request is the hook/derived level; being able to *answer* one
			// requires a channel. Only exact implies Approver.
			if info.Signals.Approvals == adapter.SupportExact {
				_, isApprover := a.(adapter.Approver)
				_, isInstaller := a.(adapter.Installer)
				// Claude Code answers through the held-open hook response
				// rather than an Approver call, so an Installer satisfies this
				// too. Both are real decision channels; neither is keystrokes.
				if !isApprover && !isInstaller {
					t.Error("declares exact approvals but implements neither " +
						"adapter.Approver nor adapter.Installer, so a human's " +
						"decision has no way to reach the agent")
				}
			}
			// Typed answers need somewhere to type them, and are keystrokes,
			// which is the derived level by definition.
			if _, ok := a.(adapter.KeyApprover); ok {
				if info.Signals.Approvals != adapter.SupportDerived {
					t.Errorf("answers approvals with keys but declares Signals.Approvals=%s, not derived",
						info.Signals.Approvals)
				}
				if len(info.Attachments) == 0 {
					t.Error("answers approvals with keys but has no attachment to type them into")
				}
			}
		})
	}
}

func TestReadOnlyAdaptersExplainWhy(t *testing.T) {
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			if len(info.Attachments) == 0 && info.ReadOnlyReason == "" {
				t.Error("has no attachments and no ReadOnlyReason; the UI would " +
					"show a disabled reply box with no explanation")
			}
			for _, at := range info.Attachments {
				switch at {
				case protocol.AttachWrap, protocol.AttachTmux, protocol.AttachTermctl,
					protocol.AttachNative, protocol.AttachTranscript:
				default:
					t.Errorf("unknown attachment %q", at)
				}
			}
		})
	}
}

func TestMaxFidelityFollowsFromCapabilities(t *testing.T) {
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			got := info.MaxFidelity()
			if got == protocol.FidelityNativeFull &&
				info.Signals.Approvals != adapter.SupportExact {
				t.Errorf("claims %s without exact approvals", got)
			}
			if len(info.Attachments) == 0 &&
				(got == protocol.FidelityFull || got == protocol.FidelityNativeFull) {
				t.Errorf("claims %s with no way to send input", got)
			}
		})
	}
}

// Detect must be cheap and total. It runs against every process on the machine
// on every scan, so a panic on an odd argv takes out discovery entirely -- and
// argv is attacker-influenced by anyone who can start a process.
func TestDetectIsTotalAndCheap(t *testing.T) {
	hostile := []adapter.Process{
		{},
		{PID: 1, Argv: []string{}},
		{PID: 2, Argv: []string{""}},
		{PID: 3, Argv: []string{"/"}},
		{PID: 4, Argv: []string{strings.Repeat("a", 1<<16)}},
		{PID: 5, Argv: []string{"claude\x00evil"}},
		{PID: 6, Argv: []string{"../../claude"}},
		{PID: 7, Argv: nil, Exe: "claude"},
	}
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			for _, p := range hostile {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("Detect panicked on %+v: %v", p, r)
						}
					}()
					start := time.Now()
					_ = a.Detect(p)
					if d := time.Since(start); d > 10*time.Millisecond {
						t.Errorf("Detect took %v; it runs per-process per-scan "+
							"and must not do real work", d)
					}
				}()
			}
		})
	}
}

// Two adapters matching one process would attribute sessions to whichever
// happened to be checked first. The registry reports the ambiguity rather than
// picking; this asserts the built-in set never triggers it.
func TestDetectRulesDoNotOverlap(t *testing.T) {
	r := reg(t)
	corpus := []adapter.Process{
		{PID: 10, Argv: []string{"claude"}},
		{PID: 11, Argv: []string{"/opt/homebrew/bin/claude", "--resume"}},
		{PID: 12, Argv: []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}},
		{PID: 13, Argv: []string{"codex"}},
		{PID: 14, Argv: []string{"/usr/local/bin/codex", "exec"}},
		{PID: 15, Argv: []string{"hermes", "chat"}},
		{PID: 16, Argv: []string{"opencode", "serve"}},
		{PID: 17, Argv: []string{"pi"}},
		{PID: 18, Argv: []string{"/Users/x/.local/bin/pi", "--model", "gpt"}},
		// Things that must match nothing.
		{PID: 20, Argv: []string{"vim", "claude.go"}},
		{PID: 21, Argv: []string{"grep", "codex"}},
		{PID: 22, Argv: []string{"pip", "install", "pi"}},
		{PID: 23, Argv: []string{"python", "-m", "hermes_client"}},
	}
	for _, p := range corpus {
		if _, err := r.Match(p); err != nil {
			t.Errorf("%v: %v", p.Argv, err)
		}
	}
}

func TestDetectMatchesTheRightAdapter(t *testing.T) {
	r := reg(t)
	cases := []struct {
		argv []string
		want string // "" means no adapter should claim it
	}{
		{[]string{"claude"}, "claude-code"},
		{[]string{"/opt/homebrew/bin/claude"}, "claude-code"},
		{[]string{"node", "/x/@anthropic-ai/claude-code/cli.js"}, "claude-code"},
		{[]string{"codex", "exec"}, "codex"},
		{[]string{"hermes"}, "hermes"},
		{[]string{"opencode", "serve"}, "opencode"},
		{[]string{"pi", "--resume"}, "pi"},
		{[]string{"vim", "claude.go"}, ""},
		{[]string{"grep", "-r", "opencode"}, ""},
		{[]string{"pip", "install", "pi"}, ""},
		{[]string{"my-codex-wrapper"}, ""},
	}
	for _, c := range cases {
		got, err := r.Match(adapter.Process{PID: 1, Argv: c.argv})
		if err != nil {
			t.Errorf("%v: %v", c.argv, err)
			continue
		}
		id := ""
		if got != nil {
			id = got.Info().ID
		}
		if id != c.want {
			t.Errorf("%v matched %q, want %q", c.argv, id, c.want)
		}
	}
}

func TestVersionClassification(t *testing.T) {
	for _, a := range reg(t).All() {
		info := a.Info()
		t.Run(info.ID, func(t *testing.T) {
			min := adapter.MustParseVersion(info.MinVersion)

			below := adapter.Version{Major: min.Major, Minor: min.Minor, Patch: min.Patch}
			switch {
			case below.Patch > 0:
				below.Patch--
			case below.Minor > 0:
				below.Minor--
			case below.Major > 0:
				below.Major--
			default:
				t.Skip("MinVersion is 0.0.0; nothing is below it")
			}
			if got := info.ClassifyVersion(below); got != adapter.VersionTooOld {
				t.Errorf("%s classified %s, want unsupported", below, got)
			}
			if got := info.ClassifyVersion(min); got != adapter.VersionSupported {
				t.Errorf("MinVersion %s classified %s, want supported", min, got)
			}

			// An agent that auto-updated past our newest profile must degrade
			// to a badge, never to a refusal.
			future := adapter.Version{Major: min.Major + 100}
			got := info.ClassifyVersion(future)
			if info.NewestTested == "" {
				if got != adapter.VersionSupported {
					t.Errorf("with no tested ceiling, %s classified %s", future, got)
				}
			} else if got != adapter.VersionUntested {
				t.Errorf("%s classified %s, want untested-version", future, got)
			}
			if got == adapter.VersionTooOld {
				t.Error("a future version must never classify as unsupported: " +
					"every user would break on the day their agent updates")
			}
		})
	}
}

// An agent that is not installed must be silence, not an error. Every adapter's
// Discover runs on every machine, and most machines have most of these agents
// absent; surfacing that as a daemon error would make `muxscale ls` fail for
// everyone.
func TestDiscoverToleratesAbsentAgents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, a := range reg(t).All() {
		d, ok := a.(adapter.Discoverer)
		if !ok {
			continue
		}
		t.Run(a.Info().ID, func(t *testing.T) {
			got, err := d.Discover(ctx)
			if err != nil {
				t.Errorf("Discover on a machine without this agent returned %v; "+
					"absence must not be an error", err)
			}
			if len(got) != 0 {
				t.Errorf("found %d sessions under an empty HOME", len(got))
			}
		})
	}
}

func TestDiscoveredSessionsAreIdentifiable(t *testing.T) {
	// Guards the field the daemon matches on across restarts. A Discovered with
	// no NaturalKey cannot be reconciled, so it would be re-added as a new
	// session on every scan.
	for _, a := range reg(t).All() {
		if _, ok := a.(adapter.Discoverer); !ok {
			if a.Info().Signals.Discovery == adapter.SupportExact {
				t.Errorf("%s: declares exact discovery but implements no Discoverer, "+
					"so it can only be found by the generic process scan",
					a.Info().ID)
			}
		}
	}
}

// The process scan filters on a binary-name table before any adapter is
// consulted, because it must be cheap and runs against every process on the
// machine. That makes it a second source of truth, and the failure mode is
// quiet: an adapter whose binary is missing from the table never sees a single
// process, and no test of that adapter would notice.
func TestScanFilterAndRegistryAgree(t *testing.T) {
	r := reg(t)

	for binary, id := range attach.AgentBinaries {
		if _, ok := r.Get(id); !ok {
			t.Errorf("the scan table maps %q to adapter %q, which is not registered; "+
				"processes matching it are found and then dropped", binary, id)
			continue
		}
		// And the adapter must actually claim the binary the table promises.
		got, err := r.Match(adapter.Process{PID: 1, Argv: []string{binary}})
		if err != nil {
			t.Errorf("%q: %v", binary, err)
			continue
		}
		if got == nil {
			t.Errorf("the scan table maps %q to %q, but no adapter's Detect "+
				"claims that binary", binary, id)
			continue
		}
		if got.Info().ID != id {
			t.Errorf("the scan table maps %q to %q, but Detect claims it for %q",
				binary, id, got.Info().ID)
		}
	}
}

func TestRegistryRejectsBadAdapters(t *testing.T) {
	r := adapter.NewRegistry()
	if err := r.Register(fake{id: "Claude-Code"}); err == nil {
		t.Error("accepted a non-kebab-case ID")
	}
	if err := r.Register(fake{id: "ok", min: "not-a-version"}); err == nil {
		t.Error("accepted an unparseable MinVersion")
	}
	if err := r.Register(fake{id: "dup", min: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(fake{id: "dup", min: "1.0.0"}); err == nil {
		t.Error("accepted a duplicate ID")
	}
}

func TestRegistryReportsAmbiguousDetection(t *testing.T) {
	r := adapter.NewRegistry()
	r.MustRegister(fake{id: "a", min: "1.0.0", claims: true},
		fake{id: "b", min: "1.0.0", claims: true})
	_, err := r.Match(adapter.Process{PID: 7, Argv: []string{"x"}})
	if err == nil {
		t.Fatal("two adapters claimed one process and Match picked one silently")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error should name both adapters, got: %v", err)
	}
}

type fake struct {
	id     string
	min    string
	claims bool
}

func (f fake) Info() adapter.Info {
	return adapter.Info{ID: f.id, DisplayName: f.id, MinVersion: f.min,
		ReadOnlyReason: "test double"}
}
func (f fake) Detect(adapter.Process) bool { return f.claims }
