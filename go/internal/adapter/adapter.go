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
	"strings"
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
	PID  int
	PPID int
	Exe  string   // resolved executable path, may be empty
	Argv []string // full argv, argv[0] included
	Cwd  string   // may be empty if unreadable
	TTY  string   // controlling terminal, may be empty

	// Started is when the process began, as the process table reports it. A
	// transcript join needs it because resuming a session reuses a file that
	// predates the process; without the start time a resumed session and a
	// fresh one in the same project are indistinguishable.
	Started time.Time

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
	// Name is a person's own label for the session, when the agent's index
	// has one. Wars over Title: an automatic title may change, a rename must
	// not be overwritten by it.
	Name  string
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

// Meta is session-level metadata recovered from a transcript.
//
// It lives here rather than in one adapter because every agent's file says
// some subset of the same things, and the daemon copying a title or a model
// onto a session is not Claude Code's business. Adapters leave a field empty
// when their format does not carry it.
type Meta struct {
	SessionID string
	Cwd       string
	GitBranch string
	Model     string
	Version   string
	Title     string

	// CustomTitle is the last name a person set (Claude Code's /rename,
	// omp's title_change with source "user"), nil when the transcript holds
	// none. A pointer because clearing a name has to be told apart from
	// never having had one.
	CustomTitle *string
	LastPrompt  string

	// Queue is the agent's queue of messages not yet taken up, whole, when
	// it changed; nil when it did not. A pointer for the same reason as
	// CustomTitle: an emptied queue is news, an unchanged one is not.
	Queue *[]protocol.QueuedMessage
}

// Merge folds an observation into m, keeping fields the observation omitted.
// Every transcript format reports a subset per line, so a chunk that carried
// only a tool result must not erase the title an earlier chunk established.
func (m *Meta) Merge(o Meta) {
	if o.SessionID != "" {
		m.SessionID = o.SessionID
	}
	if o.Cwd != "" {
		m.Cwd = o.Cwd
	}
	if o.GitBranch != "" {
		m.GitBranch = o.GitBranch
	}
	if o.Model != "" {
		m.Model = o.Model
	}
	if o.Version != "" {
		m.Version = o.Version
	}
	if o.Title != "" {
		m.Title = o.Title
	}
	if o.CustomTitle != nil {
		m.CustomTitle = o.CustomTitle
	}
	if o.LastPrompt != "" {
		m.LastPrompt = o.LastPrompt
	}
	if o.Queue != nil {
		m.Queue = o.Queue
	}
}

// Empty reports whether the observation carries nothing to apply.
func (m Meta) Empty() bool {
	return m.Title == "" && m.CustomTitle == nil && m.GitBranch == "" &&
		m.Model == "" && m.Version == "" && m.Cwd == "" && m.SessionID == "" &&
		m.Queue == nil
}

// MetaSource is implemented by transcript streams that recover metadata.
//
// Separate from TranscriptStream because reading a session's title does not
// require its conversation, and because not every format has one. Meta reports
// what changed since the previous call, not the cumulative value: a quiet
// session must produce no row updates, and a chunk that carried only a tool
// result must not erase a title observed earlier.
type MetaSource interface {
	Meta() Meta
}

// PacedStream is implemented by streams with a preferred poll cadence. The
// manager falls back to its own default when it is absent.
type PacedStream interface {
	Interval() time.Duration
}

// EventReporter is implemented by transcript streams that can name the agent's
// own most recent event.
//
// Some formats carry the state evidence in records that are not themselves
// transcript events -- Codex writes task_started and task_complete, which are
// lifecycle, not conversation. State inference needs the name of the last
// such record; without this it sees only rendered events and can never match
// "task_complete", which is the one signal that matters.
type EventReporter interface {
	LastEvent() string
}

// SeekableStream is implemented by streams that can skip to the end of what
// the store already holds, so a restart does not re-read a large transcript.
type SeekableStream interface {
	SeekEnd(fromSeq uint64) error
}

// Locator joins a running process to its transcript.
//
// This is two questions with different answers, kept apart on purpose:
//
//   - TranscriptForProcess asks for a fact the agent's own runtime recorded --
//     Claude Code's pid file, omp's terminal breadcrumb. It follows /resume and
//     /clear inside a live process and cannot pick a neighbour's conversation.
//   - MatchTranscript is a guess when no such fact exists. It takes the set of
//     paths already claimed by other live sessions so two agents in one project
//     do not both adopt the same file.
//
// Returning "" from either is valid and means "no transcript", not an error.
type Locator interface {
	TranscriptForProcess(Process) string
	MatchTranscript(cwd string, started time.Time, claimed map[string]bool) string
}

// Liveness reports whether the agent's own runtime still owns a session, when
// that can be known without a process: omp's writer lease, for example.
//
// The second return says whether the answer is known at all. An adapter with a
// transcript but no liveness signal returns false, false -- and the manager
// then relies on the process table, as it always has.
type Liveness interface {
	Live(ref string) (live bool, known bool)
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

	// Live is whether the agent's own runtime still owns the session, when an
	// adapter can answer that without the process table (see Liveness). Nil
	// means unknown; adapters must not read nil as "dead".
	Live *bool
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

// KeyApprover is implemented by adapters whose agent asks at its own keyboard
// and reports that it is asking, but cannot take an answer any other way: the
// report arrives through a hook that does not wait for a reply (an approval
// with source "hook", spec §5.7). The answer is the agent's own keys for its
// own prompt, typed into its terminal.
//
// Same rule as Approver: these are the keys for a decision a person made.
type KeyApprover interface {
	ApprovalKeys(req protocol.ApprovalRequest, d protocol.ApprovalDecision) ([]byte, error)
}

// HookKind is a normalized event an agent's installed integration reports.
//
// The daemon's state machine speaks these, not any one agent's event names.
// Claude Code calls a finished turn `Stop`; omp calls it `turn_end`; the
// session is WAITING_INPUT either way. Keeping the translation in the adapter
// is what lets a second event channel exist without a second state machine.
type HookKind string

const (
	HookSessionStart  HookKind = "session_start"
	HookTurnStart     HookKind = "turn_start"
	HookUserPrompt    HookKind = "user_prompt"
	HookToolCall      HookKind = "tool_call"
	HookToolResult    HookKind = "tool_result"
	HookStop          HookKind = "stop"
	HookIdle          HookKind = "idle"
	HookNotification  HookKind = "notification"
	HookSessionEnd    HookKind = "session_end"
	HookCompaction    HookKind = "compaction"
	HookSubagentStart HookKind = "subagent_start"
	HookSubagentStop  HookKind = "subagent_stop"
	// HookApproval is a blocking question the agent is waiting on. The
	// daemon holds the HTTP request open while a human answers.
	HookApproval HookKind = "approval"
	// HookElicit is the same, for a typed form rather than a yes/no.
	HookElicit HookKind = "elicitation"
	// HookApprovalShown is an approval prompt the agent is showing at its own
	// keyboard, reported without waiting for an answer. The answer is typed
	// (see KeyApprover).
	HookApprovalShown HookKind = "approval_shown"
	// HookQueue reports the agent's queue of messages not yet taken up, whole
	// (HookEvent.Queue).
	HookQueue HookKind = "queue"
	// HookApprovalClosed is that prompt going away: answered at the keyboard,
	// by keys muxscale typed, or abandoned.
	HookApprovalClosed HookKind = "approval_closed"
	// HookNotice is a valid event with no state consequence. It is accepted
	// and logged rather than rejected: agents add events without warning, and
	// an unknown event is not an error.
	HookNotice HookKind = "notice"
)

// HookEvent is an agent event translated into muxscale's vocabulary.
type HookEvent struct {
	AgentID string
	Kind    HookKind

	// SessionID is the agent's own session identifier (its NaturalKey).
	SessionID string
	// SessionFile is the transcript, when the transport names one. It is the
	// strongest join there is: Claude Code sends it in every payload, omp's
	// extension reads it from its session manager.
	SessionFile string
	Cwd         string

	PromptID     string
	EventName    string // the agent's own name, for evidence and debugging
	AgentVersion string
	Model        string

	ToolName   string
	ToolUseID  string
	ToolInput  map[string]any
	ToolOutput string
	IsError    bool

	IsSidechain bool
	EndReason   string

	// Approved and Reason describe how an approval prompt closed
	// (HookApprovalClosed); Reason is also why a prompt was raised.
	Approved bool
	Reason   string

	// Queue is the whole queue, with HookQueue.
	Queue *[]protocol.QueuedMessage
}

// Subject renders a short human-readable summary for a notification body.
// It must be terse: this lands on a lock screen.
//
// The literal arguments are what is shown, never the agent's description of
// them -- the structural defence against a repo persuading an agent to ask
// for something harmless-sounding.
func (e HookEvent) Subject() string {
	switch e.ToolName {
	case "":
		return e.EventName
	case "bash", "Bash":
		if cmd, ok := e.ToolInput["command"].(string); ok {
			return "bash: " + collapseWS(cmd, 80)
		}
	case "edit", "Edit", "write", "Write", "NotebookEdit":
		path, _ := e.ToolInput["path"].(string)
		if path == "" {
			path, _ = e.ToolInput["file_path"].(string)
		}
		if path != "" {
			return e.ToolName + " " + shortenPath(path)
		}
	case "read", "Read":
		path, _ := e.ToolInput["path"].(string)
		if path == "" {
			path, _ = e.ToolInput["file_path"].(string)
		}
		if path != "" {
			return "read " + shortenPath(path)
		}
	}
	return e.ToolName
}

func collapseWS(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// shortenPath keeps the last two segments, which is what identifies a file to
// a human glancing at a phone.
func shortenPath(p string) string {
	parts := strings.Split(strings.TrimSuffix(p, "/"), "/")
	if len(parts) <= 2 {
		return p
	}
	return ".../" + strings.Join(parts[len(parts)-2:], "/")
}

// HookCodec is implemented by adapters whose agent can call muxscale directly
// -- HTTP hooks, a locally installed extension POSTing events.
//
// Decode translates the agent's payload; EncodeReply translates a human's
// decision back. EncodeReply is called with a nil decision when there is
// nothing to decide, and must return whatever that agent reads as "no
// decision" (fail open). Every error path in the daemon uses that same value.
type HookCodec interface {
	HookPath() string
	DecodeHook(body []byte) (HookEvent, error)
	EncodeHookReply(e HookEvent, d *protocol.ApprovalDecision) any
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
