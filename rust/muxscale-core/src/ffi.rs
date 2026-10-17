//! The FFI surface the iOS and Android apps call.
//!
//! Three rules govern everything here, because this boundary is where a mistake
//! stops being a bug and becomes a crash in someone's hands:
//!
//!  1. **Nothing panics across the boundary.** The release profile aborts on
//!     panic, and unwinding through FFI is undefined behaviour, so every
//!     fallible operation returns a typed error instead. Out-of-range reads
//!     return blanks rather than indexing.
//!  2. **The app never sees raw bytes it must interpret.** JSON goes in, typed
//!     values come out. A renderer that has to parse protocol on the UI thread
//!     is a renderer that drops frames.
//!  3. **Errors say what to do.** A message surfaced in an app is read by a
//!     user, not a developer.

use std::sync::Mutex;

use crate::e2e;
use crate::grid::{Delta, Grid, Snapshot};
use crate::protocol::{Session, TranscriptEvent};

/// Errors the app can act on.
///
/// Deliberately few and coarse. A phone UI can do one of three things with a
/// failure -- retry, re-pair, or show a message -- so the variants map to those
/// rather than mirroring every internal cause.
#[derive(Debug, thiserror::Error, uniffi::Error)]
pub enum CoreError {
    /// The payload from the daemon could not be understood. Usually a version
    /// mismatch, so the message says so.
    #[error("{message}")]
    Protocol { message: String },

    /// A cryptographic operation failed: tampering, replay, or a stale session.
    /// The app should re-establish rather than retry the same bytes.
    #[error("{message}")]
    Crypto { message: String },

    /// An operation was attempted in the wrong order.
    #[error("{message}")]
    State { message: String },
}

impl From<e2e::Error> for CoreError {
    fn from(e: e2e::Error) -> Self {
        match e {
            e2e::Error::NotReady => CoreError::State {
                message: "the secure channel is not established yet".into(),
            },
            other => CoreError::Crypto {
                message: other.to_string(),
            },
        }
    }
}

fn protocol_err(context: &str, e: impl std::fmt::Display) -> CoreError {
    CoreError::Protocol {
        message: format!("could not read {context} from the daemon ({e}). This usually means the app and the daemon are different versions."),
    }
}

// ---- terminal grid ----

/// One cell, flattened for the renderer.
///
/// The three colour cases are pre-resolved here rather than in the app: a
/// renderer should not have to know that -1 means default and that a flag bit
/// distinguishes palette from truecolour.
#[derive(Debug, Clone, uniffi::Record)]
pub struct RenderCell {
    /// The character to draw. A continuation cell of a wide character is empty.
    pub text: String,
    /// Palette index 0-255, if the foreground is a palette colour.
    pub fg_palette: Option<u8>,
    /// Packed 0xRRGGBB, if the foreground is truecolour.
    pub fg_rgb: Option<u32>,
    pub bg_palette: Option<u8>,
    pub bg_rgb: Option<u32>,
    pub bold: bool,
    pub italic: bool,
    pub underline: bool,
    pub reverse: bool,
    pub dim: bool,
    pub strike: bool,
    /// True for the second column of a double-width character, which the
    /// renderer must skip rather than draw.
    pub wide_continuation: bool,
}

/// One row of cells.
#[derive(Debug, Clone, uniffi::Record)]
pub struct RenderRow {
    pub y: u32,
    pub cells: Vec<RenderCell>,
}

/// Where the cursor is.
#[derive(Debug, Clone, uniffi::Record)]
pub struct CursorPosition {
    pub x: u32,
    pub y: u32,
    pub hidden: bool,
}

/// The terminal view model.
///
/// Held across the FFI boundary so the delta path never touches the JS thread:
/// the app hands over JSON and asks for rows to draw.
#[derive(uniffi::Object)]
pub struct TerminalView {
    inner: Mutex<Grid>,
}

#[uniffi::export]
impl TerminalView {
    /// Creates a view of the given size.
    #[uniffi::constructor]
    pub fn new(cols: u32, rows: u32) -> Self {
        TerminalView {
            inner: Mutex::new(Grid::new(cols as usize, rows as usize)),
        }
    }

    /// Replaces the screen from a snapshot.
    pub fn apply_snapshot_json(&self, json: &str) -> Result<(), CoreError> {
        let snap: Snapshot =
            serde_json::from_str(json).map_err(|e| protocol_err("a screen snapshot", e))?;
        self.locked()?.apply_snapshot(&snap);
        Ok(())
    }

    /// Applies changed rows.
    ///
    /// If this ever fails, the correct recovery is to request a fresh snapshot
    /// rather than retrying: the delta stream is only meaningful in order.
    pub fn apply_delta_json(&self, json: &str) -> Result<(), CoreError> {
        let delta: Delta =
            serde_json::from_str(json).map_err(|e| protocol_err("a screen update", e))?;
        self.locked()?.apply_delta(&delta);
        Ok(())
    }

    /// Returns rows for the renderer.
    ///
    /// `from` and `count` clamp to the grid rather than erroring: a renderer
    /// racing a resize is normal, and crashing the app over a cosmetic race
    /// would be absurd.
    pub fn rows(&self, from: u32, count: u32) -> Result<Vec<RenderRow>, CoreError> {
        let g = self.locked()?;
        let start = (from as usize).min(g.rows);
        let end = (start + count as usize).min(g.rows);

        let mut out = Vec::with_capacity(end - start);
        for y in start..end {
            let mut cells = Vec::with_capacity(g.cols);
            for x in 0..g.cols {
                let c = g.at(x, y);
                cells.push(RenderCell {
                    text: if c.wide {
                        String::new()
                    } else {
                        c.ch.to_string()
                    },
                    fg_palette: c.attr.fg_palette(),
                    fg_rgb: c.attr.fg_rgb().map(pack_rgb),
                    bg_palette: c.attr.bg_palette(),
                    bg_rgb: c.attr.bg_rgb().map(pack_rgb),
                    bold: c.attr.bold,
                    italic: c.attr.italic,
                    underline: c.attr.underline,
                    reverse: c.attr.reverse,
                    dim: c.attr.dim,
                    strike: c.attr.strike,
                    wide_continuation: c.wide,
                });
            }
            out.push(RenderRow { y: y as u32, cells });
        }
        Ok(out)
    }

    /// Renders the screen as plain text, for accessibility and copy.
    pub fn text(&self) -> Result<String, CoreError> {
        Ok(self.locked()?.text())
    }

    /// Renders one row as plain text.
    pub fn row_text(&self, y: u32) -> Result<String, CoreError> {
        Ok(self.locked()?.row_text(y as usize))
    }

    /// The current cursor position.
    pub fn cursor(&self) -> Result<CursorPosition, CoreError> {
        let g = self.locked()?;
        Ok(CursorPosition {
            x: g.cursor_x as u32,
            y: g.cursor_y as u32,
            hidden: g.cursor_off,
        })
    }

    /// The grid dimensions, as (cols, rows).
    pub fn size(&self) -> Result<Vec<u32>, CoreError> {
        let g = self.locked()?;
        Ok(vec![g.cols as u32, g.rows as u32])
    }
}

impl TerminalView {
    fn locked(&self) -> Result<std::sync::MutexGuard<'_, Grid>, CoreError> {
        // A poisoned mutex means another thread panicked while holding it.
        // Surfacing that as an error beats propagating a panic across FFI.
        self.inner.lock().map_err(|_| CoreError::State {
            message: "the terminal view is in an inconsistent state; reopen the session".into(),
        })
    }
}

fn pack_rgb((r, g, b): (u8, u8, u8)) -> u32 {
    ((r as u32) << 16) | ((g as u32) << 8) | b as u32
}

// ---- protocol decoding ----

/// Decodes the session list the daemon returns.
///
/// Done here rather than in TypeScript so there is one definition of the wire
/// format, and so a schema change surfaces as a typed error rather than an
/// undefined field three screens later.
#[uniffi::export]
pub fn decode_sessions(json: &str) -> Result<Vec<SessionSummary>, CoreError> {
    #[derive(serde::Deserialize)]
    struct Envelope {
        #[serde(default)]
        sessions: Vec<Session>,
    }
    // Accept both the envelope and a bare array; the daemon has used both.
    let sessions: Vec<Session> = match serde_json::from_str::<Envelope>(json) {
        Ok(e) => e.sessions,
        Err(_) => serde_json::from_str(json).map_err(|e| protocol_err("the session list", e))?,
    };
    Ok(sessions.iter().map(SessionSummary::from).collect())
}

/// A session, flattened for the list screen.
///
/// `can_input` and `is_authoritative` are resolved here so the UI cannot get
/// them wrong: a compose bar shown for a read-only session, or a guess
/// presented as a fact, are both failures the app should be unable to make.
#[derive(Debug, Clone, uniffi::Record)]
pub struct SessionSummary {
    pub id: String,
    pub agent_id: String,
    pub title: String,
    pub cwd: String,
    pub git_branch: String,
    pub model: String,
    pub state: String,
    /// True for states a human needs to resolve; these sort to the top.
    pub is_waiting: bool,
    pub is_terminal: bool,
    /// False for read-only sessions. The compose bar renders from this.
    pub can_input: bool,
    /// False when the state was inferred rather than reported by the agent.
    /// The UI labels these differently.
    pub is_authoritative: bool,
    pub state_reason: String,
    pub prompt_id: String,
    pub pending_approval_subject: Option<String>,
}

impl From<&Session> for SessionSummary {
    fn from(s: &Session) -> Self {
        SessionSummary {
            id: s.id.clone(),
            agent_id: s.agent_id.clone(),
            // What a list shows: the name a person gave it, else the agent's own.
            title: if s.name.is_empty() { s.title.clone() } else { s.name.clone() },
            cwd: s.cwd.clone(),
            git_branch: s.git_branch.clone(),
            model: s.model.clone(),
            state: format!("{:?}", s.state),
            is_waiting: s.state.waiting(),
            is_terminal: s.state.terminal(),
            can_input: s.fidelity.can_input(),
            is_authoritative: s
                .guess
                .as_ref()
                .map(|g| g.is_authoritative())
                .unwrap_or(false),
            state_reason: s
                .guess
                .as_ref()
                .map(|g| g.reason.clone())
                .unwrap_or_default(),
            prompt_id: s.prompt_id.clone(),
            pending_approval_subject: s.pending_approval.as_ref().map(|a| a.subject.clone()),
        }
    }
}

/// A transcript entry, flattened for the conversation view.
#[derive(Debug, Clone, uniffi::Record)]
pub struct TranscriptItem {
    pub seq: u64,
    pub id: String,
    pub parent_id: String,
    pub kind: String,
    pub text: String,
    pub tool_name: String,
    pub tool_output: String,
    pub is_error: bool,
    pub is_sidechain: bool,
    pub truncated: bool,
}

/// Decodes a transcript page.
#[uniffi::export]
pub fn decode_transcript(json: &str) -> Result<Vec<TranscriptItem>, CoreError> {
    #[derive(serde::Deserialize)]
    struct Envelope {
        #[serde(default)]
        events: Vec<TranscriptEvent>,
    }
    let events: Vec<TranscriptEvent> = match serde_json::from_str::<Envelope>(json) {
        Ok(e) => e.events,
        Err(_) => serde_json::from_str(json).map_err(|e| protocol_err("the transcript", e))?,
    };
    Ok(events
        .into_iter()
        .map(|e| TranscriptItem {
            seq: e.seq,
            id: e.id,
            parent_id: e.parent_id,
            kind: format!("{:?}", e.kind),
            text: e.text,
            tool_name: e.tool_name,
            tool_output: e.tool_output,
            is_error: e.is_error,
            is_sidechain: e.is_sidechain,
            truncated: e.truncated,
        })
        .collect())
}

// ---- pairing ----

/// The device side of a pairing handshake.
///
/// The app drives this: scan the QR code, exchange two messages, show the
/// words, and let the human confirm they match what the host displays.
#[derive(uniffi::Object)]
pub struct Pairing {
    inner: Mutex<PairingState>,
}

struct PairingState {
    handshake: Option<e2e::Handshake>,
    session: Option<e2e::Session>,
    device_public: Vec<u8>,
}

#[uniffi::export]
impl Pairing {
    /// Starts pairing with the daemon key from a scanned invite.
    #[uniffi::constructor]
    pub fn new(daemon_public_base64: &str) -> Result<Self, CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let daemon_public =
            STANDARD
                .decode(daemon_public_base64)
                .map_err(|_| CoreError::Crypto {
                    message: "that QR code is not a valid muxscale invite".into(),
                })?;

        let device = e2e::KeyPair::generate()?;
        let device_public = device.public.clone();
        let handshake = e2e::Handshake::new(e2e::Role::Device, &device, Some(&daemon_public))?;

        Ok(Pairing {
            inner: Mutex::new(PairingState {
                handshake: Some(handshake),
                session: None,
                device_public,
            }),
        })
    }

    /// Produces the first handshake message to send to the daemon.
    pub fn first_message_base64(&self) -> Result<String, CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let mut st = self.locked()?;
        let hs = st.handshake.as_mut().ok_or(CoreError::State {
            message: "pairing has already completed".into(),
        })?;
        Ok(STANDARD.encode(hs.write_message(&[])?))
    }

    /// Consumes the daemon's reply, completing the handshake.
    pub fn accept_reply_base64(&self, reply: &str) -> Result<(), CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let raw = STANDARD.decode(reply).map_err(|_| CoreError::Protocol {
            message: "the daemon's reply was malformed".into(),
        })?;
        let mut st = self.locked()?;
        let hs = st.handshake.as_mut().ok_or(CoreError::State {
            message: "pairing has already completed".into(),
        })?;
        hs.read_message(&raw)?;
        if !hs.complete() {
            return Err(CoreError::State {
                message: "the handshake did not complete".into(),
            });
        }
        Ok(())
    }

    /// The words to show the user for comparison against the host's screen.
    ///
    /// This is the step that defeats a compromised relay, so it is deliberately
    /// available before the session is usable: the human confirms first.
    pub fn verification_words(&self) -> Result<Vec<String>, CoreError> {
        let st = self.locked()?;
        let hs = st.handshake.as_ref().ok_or(CoreError::State {
            message: "pairing has already been confirmed".into(),
        })?;
        Ok(e2e::sas(hs.channel_binding())?
            .into_iter()
            .map(String::from)
            .collect())
    }

    /// Confirms the words matched and promotes the handshake to a usable
    /// session.
    ///
    /// Requiring an explicit call means the app cannot accidentally skip the
    /// human check: without it there is no session to send anything through.
    pub fn confirm(&self) -> Result<(), CoreError> {
        let mut st = self.locked()?;
        let hs = st.handshake.take().ok_or(CoreError::State {
            message: "pairing has already been confirmed".into(),
        })?;
        st.session = Some(hs.into_session()?);
        Ok(())
    }

    /// This device's public key, to register with the daemon.
    pub fn device_public_base64(&self) -> Result<String, CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        Ok(STANDARD.encode(&self.locked()?.device_public))
    }

    /// Encrypts a payload for the daemon.
    pub fn seal_base64(&self, plaintext: &str, header: &str) -> Result<String, CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let mut st = self.locked()?;
        let sess = st.session.as_mut().ok_or(CoreError::State {
            message: "confirm the verification words before sending anything".into(),
        })?;
        Ok(STANDARD.encode(sess.seal(plaintext.as_bytes(), header.as_bytes())?))
    }

    /// Decrypts a payload from the daemon, verifying it was addressed here.
    pub fn open_base64(&self, ciphertext: &str, header: &str) -> Result<String, CoreError> {
        use base64::{engine::general_purpose::STANDARD, Engine};
        let raw = STANDARD
            .decode(ciphertext)
            .map_err(|_| CoreError::Protocol {
                message: "the message was malformed".into(),
            })?;
        let mut st = self.locked()?;
        let sess = st.session.as_mut().ok_or(CoreError::State {
            message: "confirm the verification words before reading anything".into(),
        })?;
        let pt = sess.open(&raw, header.as_bytes())?;
        String::from_utf8(pt).map_err(|_| CoreError::Protocol {
            message: "the decrypted message was not valid text".into(),
        })
    }
}

impl Pairing {
    fn locked(&self) -> Result<std::sync::MutexGuard<'_, PairingState>, CoreError> {
        self.inner.lock().map_err(|_| CoreError::State {
            message: "pairing is in an inconsistent state; start again".into(),
        })
    }
}

/// The protocol version this core speaks, for the app to report in diagnostics.
#[uniffi::export]
pub fn protocol_version() -> u32 {
    crate::PROTOCOL_VERSION
}

/// The core's version string.
#[uniffi::export]
pub fn core_version() -> String {
    crate::VERSION.to_string()
}
