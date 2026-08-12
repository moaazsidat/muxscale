// Package adapter defines the seam between the daemon and a specific coding
// agent. Everything agent-specific lives behind it; the daemon core, the wire
// protocol, and the apps know only what is declared here.
//
// The dependency direction is adapter implementation -> adapter -> protocol,
// never the reverse. An import of internal/adapter/<agent> anywhere outside
// that agent's own package and the registry wiring is a bug: it means the seam
// leaked, which is exactly what M3.5 exists to catch.
//
// # Why capabilities are declared rather than discovered
//
// Agents differ enormously in what they will tell you. Claude Code pushes
// authoritative hook events; hermes only has a SQLite file; Codex writes
// rollout files after the fact. The daemon has to know which of those it is
// dealing with *before* it decides whether to notify someone, because the cost
// of a confident-sounding wrong answer is a user who turns notifications off.
//
// So each adapter states, in Info, what signals it can actually produce. The
// conformance test then checks the declaration against the implementation --
// an adapter claiming exact state detection without implementing StateInferrer
// fails the build rather than silently degrading in production.
package adapter

import (
	"context"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// Support grades how good a signal is, which is what gates notification.
type Support string

const (
	// SupportNone: the adapter cannot produce this signal at all.
	SupportNone Support = "none"
	// SupportDerived: inferred from indirect evidence -- transcript quiescence,
	// output patterns, process state. Carries confidence below 1 and is subject
	// to the notification threshold in spec §4.6.
	SupportDerived Support = "derived"
	// SupportExact: the agent itself said so, via a hook, an extension, or a
	// native API. Safe to notify on without further corroboration.
	SupportExact Support = "exact"
)

// Ranks Support so the daemon can prefer the better of two signals for the
// same session. See daemon.Manager.Reconcile, which faces exactly this when a
// session is found by both process scan and hooks.
func (s Support) Rank() int {
	switch s {
	case SupportExact:
		return 2
	case SupportDerived:
		return 1
	default:
		return 0
	}
}

// Signals is what an adapter can observe about a session it did not launch.
// This is the §4.3.7 capability table, expressed as data so it can be tested
// rather than asserted in prose.
type Signals struct {
	// Discovery: can sessions be found at all. In practice always at least
	// derived, because the process scan works for anything with a recognisable
	// argv, but an adapter with a native session list reports exact.
	Discovery Support

	// State: whether the agent is running, blocked, or idle. The signal the
	// whole product rests on.
	State Support

	// Transcript: the structured conversation.
	Transcript Support

	// Approvals: whether the agent will ask muxscale a blocking question and
	// wait for an answer. Exact means a real decision channel exists; derived
	// means an approval was noticed but can only be answered as keystrokes.
	Approvals Support
}

// Info is an adapter's static self-description.
type Info struct {
	// ID is the stable identifier used in config, the wire protocol, and
	// storage. Lowercase kebab-case, e.g. "claude-code".
	ID string

	// DisplayName is what a human sees.
	DisplayName string

	// MinVersion is the floor below which the adapter refuses its richer
	// features and falls back to read-only observation. Never a hard refusal:
	// an old agent should still appear in the list.
	MinVersion string

	// NewestTested is the highest version this build has fixtures for. Versions
	// above it are used with the newest profile and badged, never rejected --
	// failing closed on an unrecognised version would break every user on the
	// day their agent auto-updates.
	NewestTested string

	// Signals declares what this adapter can observe. Checked against the
	// implemented optional interfaces by the conformance test.
	Signals Signals

	// Attachments are the mechanisms through which remote input can reach this
	// agent, best first. Empty means the agent is observable but cannot be
	// answered remotely -- which is a legitimate and common state, not a defect.
	Attachments []protocol.Attachment

	// ReadOnlyReason explains, in one sentence for a human, why remote input is
	// unavailable when Attachments is empty. Shown in the UI instead of an
	// unexplained disabled button.
	ReadOnlyReason string
}

// MaxFidelity is the best fidelity this adapter can reach, derived from its
// declared signals and attachments rather than stated separately -- two fields
// that must agree are two fields that will eventually disagree.
func (i Info) MaxFidelity() protocol.Fidelity {
	switch {
	case i.Signals.Approvals == SupportExact && len(i.Attachments) > 0:
		return protocol.FidelityNativeFull
	case len(i.Attachments) > 0:
		return protocol.FidelityFull
	case i.Signals.Transcript != SupportNone:
		return protocol.FidelityStructured
	default:
		return protocol.FidelityReadOnly
	}
}

// Adapter is the minimum every agent integration implements. It is deliberately
// tiny: the interesting capabilities are optional interfaces below, because
// forcing hermes to implement an approval method it can never satisfy produces
// a stub that lies.
type Adapter interface {
	Info() Info

	// Detect reports whether a running process is a session of this agent.
	// Called for every process in the scan, so it must be cheap and must not
	// touch the filesystem or run commands.
	Detect(Process) bool
}

// Process is the subset of a process table entry adapters may match on.
// Deliberately not the OS-specific struct: an adapter that reaches for a pid
// to poke at is doing something the seam should not allow.
type Process struct {
	PID      int
	PPID     int
	Exe      string   // resolved executable path, may be empty
	Argv     []string // full argv, argv[0] included
	Cwd      string   // may be empty if unreadable
	TTY      string   // controlling terminal, may be empty
	Ancestry []string // executable names from parent to session leader
}

// Discovered is a session found by an adapter's own enumeration, as opposed to
// by the generic process scan.
type Discovered struct {
	// NaturalKey is the agent's own session identifier. Combined with the
	// adapter ID this survives daemon restarts and agent resumes, and is what
	// Reconcile matches on.
	NaturalKey string

	Cwd   string
	Title string
	Model string

	// Ref is an adapter-private handle -- a transcript path, a rollout file, a
	// database row id, a server session id. Passed back to Transcript and
	// never interpreted by the daemon.
	Ref string

	Live bool
}

// Discoverer is implemented by adapters that can enumerate sessions
// independently of the process table: by reading session files, querying a
// local server, or opening the agent's own database. This finds sessions that
// are not currently running, which the process scan by construction cannot.
type Discoverer interface {
	Discover(context.Context) ([]Discovered, error)
}

// Transcriber opens a session's structured conversation.
//
// This returns a stateful stream rather than taking a cursor argument. The
// first shape tried here was Transcript(ref, cursor) -> (events, nextCursor),
// which reads well and fits none of the adapters: Claude Code tracks a byte
// offset plus a partial-line remainder, hermes a rowid, OpenCode a live SSE
// connection that cannot be reconstructed from an integer at all. Handing each
// adapter its own cursor object is the honest version.
type Transcriber interface {
	OpenTranscript(ctx context.Context, ref string) (TranscriptStream, error)
}

// TranscriptStream yields a session's events incrementally.
type TranscriptStream interface {
	// Poll returns events that appeared since the previous call, oldest first,
	// and blocks no longer than the context allows. An empty slice with a nil
	// error means nothing new, which is the common case.
	Poll(ctx context.Context) ([]protocol.TranscriptEvent, error)

	// Close releases whatever the stream holds -- a file handle, a database
	// connection, an HTTP response body.
	Close() error
}

// StateInferrer derives a session's state. Adapters declaring
// Signals.State == SupportExact still implement this: the difference is the
// confidence they return, not whether the method exists.
type StateInferrer interface {
	InferState(StateInput) (protocol.StateGuess, bool)
}

// StateInput is the evidence available when inferring state.
type StateInput struct {
	// Ref is the adapter-private handle from Discovered.Ref, so an adapter can
	// look up whatever else it needs.
	Ref string

	// Ended and EndReason carry a terminal signal the daemon already knows
	// about, when the agent provides one.
	//
	// These are not a convenience. hermes records an end_reason, which is its
	// single exact signal amid otherwise inferred state; without a way to pass
	// it here, a finished session falls through to the silence heuristic and
	// is reported as WAITING_INPUT at 0.6 confidence -- muxscale telling you a
	// completed session needs your attention, which is the top-line risk in
	// spec §14.1.
	Ended     bool
	EndReason string

	// Event is the agent's own most recent event name, when there is one.
	// Empty for adapters with no event channel.
	Event string

	// Recent is a tail of the transcript, oldest first. May be empty.
	Recent []protocol.TranscriptEvent

	// Idle is how long since the session last produced anything.
	Idle time.Duration

	// Quiescence is the configured threshold beyond which silence is treated
	// as meaningful. Passed in rather than baked into each adapter so it stays
	// one tunable rather than five.
	Quiescence time.Duration
}

// Approver is implemented by adapters that can carry a human's answer back to
// a blocked agent.
//
// muxscale never decides an approval. Reply transmits a decision a person
// already made; there is deliberately no path by which a policy, a default, or
// a timeout can produce an "allow".
type Approver interface {
	Reply(ctx context.Context, req protocol.ApprovalRequest, d protocol.ApprovalDecision) error
}

// Installer is implemented by adapters that configure the agent to talk to
// muxscale -- writing hooks into settings.json, dropping an extension, adding
// a config stanza.
//
// The methods return a plan instead of performing the change, so that every
// mutation of a user's agent config can be displayed and confirmed before it
// happens. An Install(ctx) that just did the work would make that impossible
// to enforce at the seam, and "the CLI happens to ask first" is a property that
// survives exactly until someone adds a second caller.
//
// Implementations must be idempotent, must back up whatever they modify, and
// PlanUninstall must return the file to its prior state. The user's agent
// config is theirs; muxscale is a guest in it.
type Installer interface {
	PlanInstall(ctx context.Context) (InstallPlan, error)
	PlanUninstall(ctx context.Context) (InstallPlan, error)
	Installed(ctx context.Context) (bool, error)
}

// InstallPlan is a pending change to a user's machine.
type InstallPlan interface {
	// Empty reports that there is nothing to do, so callers can say "already
	// configured" rather than showing an empty diff.
	Empty() bool

	// Describe renders the change for a human: which file, what is added or
	// removed, and where the backup goes.
	Describe() string

	// Apply performs the change.
	Apply() error
}
