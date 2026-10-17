//! Wire types shared with the daemon.
//!
//! These mirror `go/internal/protocol`. Field names follow the Go JSON tags,
//! not Rust convention, because matching the wire is what matters.

use serde::{Deserialize, Serialize};

/// The lifecycle state of one agent session.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum SessionState {
    #[serde(rename = "STARTING")]
    Starting,
    #[serde(rename = "RUNNING")]
    Running,
    #[serde(rename = "WAITING_INPUT")]
    WaitingInput,
    #[serde(rename = "WAITING_APPROVAL")]
    WaitingApproval,
    #[serde(rename = "WAITING_EXTERNAL")]
    WaitingExternal,
    #[serde(rename = "IDLE")]
    Idle,
    #[serde(rename = "EXITED")]
    Exited,
    #[serde(rename = "CRASHED")]
    Crashed,
    #[serde(rename = "DETACHED")]
    Detached,
    #[serde(rename = "UNKNOWN")]
    Unknown,
}

impl SessionState {
    /// True for states a human needs to resolve. Drives sort order and badges.
    pub fn waiting(&self) -> bool {
        matches!(
            self,
            SessionState::WaitingInput
                | SessionState::WaitingApproval
                | SessionState::WaitingExternal
        )
    }

    /// True when no further events are expected.
    pub fn terminal(&self) -> bool {
        matches!(self, SessionState::Exited | SessionState::Crashed)
    }
}

/// What an attachment permits, which is what the UI must gate affordances on.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum Fidelity {
    #[serde(rename = "NATIVE_FULL")]
    NativeFull,
    #[serde(rename = "FULL")]
    Full,
    #[serde(rename = "STRUCTURED")]
    Structured,
    #[serde(rename = "READONLY")]
    ReadOnly,
}

impl Fidelity {
    /// Whether this session can accept input at all.
    ///
    /// The app renders its compose bar from this. An input box that silently
    /// fails is worse than none, so the capability has to be explicit rather
    /// than assumed.
    pub fn can_input(&self) -> bool {
        !matches!(self, Fidelity::ReadOnly)
    }
}

/// A state inference and the evidence behind it.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StateGuess {
    pub state: SessionState,
    #[serde(default)]
    pub confidence: f64,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub evidence: String,
}

impl StateGuess {
    /// True when the agent told us directly rather than us inferring.
    ///
    /// The app labels heuristic states differently: presenting a guess as a
    /// fact is how a notification loses a user's trust.
    pub fn is_authoritative(&self) -> bool {
        self.confidence >= 1.0
    }
}

/// The daemon's view of one agent conversation.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Session {
    pub id: String,
    pub agent_id: String,
    #[serde(default)]
    pub title: String,
    #[serde(default)]
    pub cwd: String,
    #[serde(default)]
    pub git_branch: String,
    #[serde(default)]
    pub model: String,
    #[serde(default)]
    pub attachment: String,
    pub fidelity: Fidelity,
    pub state: SessionState,
    #[serde(default)]
    pub guess: Option<StateGuess>,
    #[serde(default)]
    pub prompt_id: String,
    #[serde(default)]
    pub pending_approval: Option<ApprovalRequest>,
}

/// What kind of transcript entry this is.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub enum EventKind {
    #[serde(rename = "user_message")]
    UserMessage,
    #[serde(rename = "assistant_message")]
    AssistantMessage,
    #[serde(rename = "thinking")]
    Thinking,
    #[serde(rename = "tool_call")]
    ToolCall,
    #[serde(rename = "tool_result")]
    ToolResult,
    #[serde(rename = "approval_request")]
    ApprovalRequest,
    #[serde(rename = "approval_resolved")]
    ApprovalResolved,
    #[serde(rename = "state_change")]
    StateChange,
    #[serde(rename = "token_usage")]
    TokenUsage,
    #[serde(rename = "compaction")]
    Compaction,
    #[serde(rename = "error")]
    Error,
    #[serde(rename = "command")]
    Command,
    #[serde(rename = "command_output")]
    CommandOutput,
    #[serde(rename = "notice")]
    Notice,
}

/// One entry in a session's structured transcript.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TranscriptEvent {
    pub seq: u64,
    #[serde(default)]
    pub id: String,
    /// Makes the transcript a tree rather than a list. Claude Code and pi both
    /// model sessions as trees with forks; flattening misrepresents them.
    #[serde(default)]
    pub parent_id: String,
    #[serde(default)]
    pub session_id: String,
    pub kind: EventKind,
    #[serde(default)]
    pub is_sidechain: bool,
    #[serde(default)]
    pub text: String,
    #[serde(default)]
    pub tool_name: String,
    #[serde(default)]
    pub tool_use_id: String,
    #[serde(default)]
    pub tool_output: String,
    #[serde(default)]
    pub is_error: bool,
    #[serde(default)]
    pub truncated: bool,
}

/// A question the agent is blocked on.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ApprovalRequest {
    pub id: String,
    #[serde(default)]
    pub session_id: String,
    /// How the request was obtained. The UI labels a heuristic approval
    /// differently and requires an extra confirmation, because the option
    /// mapping there is a guess.
    #[serde(default)]
    pub source: String,
    #[serde(default)]
    pub tool_name: String,
    #[serde(default)]
    pub subject: String,
    #[serde(default)]
    pub options: Vec<String>,
    #[serde(default)]
    pub prompt_id: String,
}

impl ApprovalRequest {
    /// True when the agent asked its client a structured question and is
    /// blocking on the answer, rather than the state being inferred.
    pub fn is_native(&self) -> bool {
        self.source == "native"
    }

    /// True when the request was inferred from a prompt regex, which the UI
    /// must present with an extra confirmation step.
    pub fn is_heuristic(&self) -> bool {
        self.source == "heuristic"
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn session_state_round_trips_the_go_names() {
        let json = r#""WAITING_APPROVAL""#;
        let s: SessionState = serde_json::from_str(json).unwrap();
        assert_eq!(s, SessionState::WaitingApproval);
        assert_eq!(serde_json::to_string(&s).unwrap(), json);
    }

    #[test]
    fn waiting_and_terminal_classification() {
        assert!(SessionState::WaitingInput.waiting());
        assert!(SessionState::WaitingApproval.waiting());
        assert!(!SessionState::Running.waiting());
        assert!(SessionState::Exited.terminal());
        assert!(SessionState::Crashed.terminal());
        assert!(!SessionState::Detached.terminal());
    }

    // The compose bar is rendered from this. An input box that silently fails
    // is worse than no input box.
    #[test]
    fn fidelity_gates_input() {
        assert!(Fidelity::NativeFull.can_input());
        assert!(Fidelity::Full.can_input());
        assert!(Fidelity::Structured.can_input());
        assert!(!Fidelity::ReadOnly.can_input());
    }

    #[test]
    fn deserialises_a_real_session_payload() {
        // Shaped exactly as the daemon's /v1/sessions returns it.
        let json = r#"{
          "id":"06fz301ryw91y13e","agent_id":"claude-code",
          "title":"Explore Pi and Hermes agent platforms",
          "cwd":"/Users/sam/dev/muxscale","git_branch":"main",
          "model":"claude-opus-5","attachment":"native","fidelity":"NATIVE_FULL",
          "state":"WAITING_INPUT","prompt_id":"p1",
          "guess":{"state":"WAITING_INPUT","confidence":1,"reason":"turn complete",
                   "evidence":"hook:Stop"}
        }"#;
        let s: Session = serde_json::from_str(json).unwrap();
        assert_eq!(s.agent_id, "claude-code");
        assert_eq!(s.state, SessionState::WaitingInput);
        assert!(s.state.waiting());
        assert!(s.fidelity.can_input());
        let g = s.guess.unwrap();
        assert!(
            g.is_authoritative(),
            "a hook-sourced state is authoritative"
        );
        assert_eq!(g.evidence, "hook:Stop");
    }

    // Presenting a guess as a fact is how a notification loses a user's trust.
    #[test]
    fn inferred_states_are_not_authoritative() {
        let g = StateGuess {
            state: SessionState::WaitingInput,
            confidence: 0.6,
            reason: "quiet after an assistant message".into(),
            evidence: "transcript:idle 5m".into(),
        };
        assert!(!g.is_authoritative());
    }

    #[test]
    fn approval_source_classification() {
        let native = ApprovalRequest {
            id: "a".into(),
            session_id: String::new(),
            source: "native".into(),
            tool_name: "Bash".into(),
            subject: "rm -rf".into(),
            options: vec!["allow".into(), "deny".into()],
            prompt_id: String::new(),
        };
        assert!(native.is_native());
        assert!(!native.is_heuristic());

        let guessed = ApprovalRequest {
            source: "heuristic".into(),
            ..native.clone()
        };
        assert!(guessed.is_heuristic());
    }

    #[test]
    fn transcript_event_tree_linkage_survives() {
        let json = r#"{"seq":3,"id":"e3","parent_id":"e2","kind":"tool_call",
                       "tool_name":"Bash","tool_use_id":"t1"}"#;
        let e: TranscriptEvent = serde_json::from_str(json).unwrap();
        assert_eq!(e.parent_id, "e2");
        assert_eq!(e.kind, EventKind::ToolCall);
    }
}
