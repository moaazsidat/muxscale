//! The muxscale client core.
//!
//! Shared by the iOS and Android apps through a thin FFI layer. Everything that
//! must agree byte-for-byte with the daemon lives here rather than in
//! TypeScript: the grid model, the wire types, and (next) the Noise transport.
//!
//! Two reasons it is Rust and not JS. The grid delta path runs at up to 20
//! frames a second and must stay off the JS thread entirely. And the protocol
//! and crypto need one implementation that is tested once, not two hand-written
//! ones that drift.

// UniFFI scaffolding must be set up in the crate root, not a submodule.
uniffi::setup_scaffolding!();

pub mod e2e;
pub mod ffi;
pub mod grid;
pub mod protocol;

pub use e2e::{Handshake, KeyPair, Role, Session as E2eSession};
pub use grid::{Attr, Cell, Delta, Grid, RowRuns, Run, Snapshot};
pub use protocol::{ApprovalRequest, EventKind, Session, SessionState, TranscriptEvent};

/// The wire protocol version this core speaks.
pub const PROTOCOL_VERSION: u32 = 1;

/// The crate version, surfaced so a client can report it in diagnostics.
pub const VERSION: &str = env!("CARGO_PKG_VERSION");
