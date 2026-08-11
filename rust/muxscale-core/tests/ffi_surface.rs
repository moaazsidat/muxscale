//! Tests for the FFI surface itself.
//!
//! This boundary is where a mistake stops being a bug and becomes a crash in
//! someone's hands: the release profile aborts on panic, and unwinding through
//! FFI is undefined behaviour. So these tests are mostly about the failure
//! paths -- malformed input, wrong order, out-of-range access -- rather than the
//! happy path the unit tests already cover.

use muxscale_core::ffi::*;

#[test]
fn terminal_view_applies_a_snapshot() {
    let v = TerminalView::new(10, 3);
    v.apply_snapshot_json(
        r#"{"cols":10,"rows":3,"cx":2,"cy":1,
            "lines":[{"y":0,"r":[{"t":"hello","a":{"FG":-1,"BG":-1}}]}]}"#,
    )
    .unwrap();

    assert_eq!(v.row_text(0).unwrap(), "hello");
    let c = v.cursor().unwrap();
    assert_eq!((c.x, c.y, c.hidden), (2, 1, false));
    assert_eq!(v.size().unwrap(), vec![10, 3]);
}

/// Malformed JSON must be a typed error, never a panic.
#[test]
fn malformed_payloads_error_rather_than_panic() {
    let v = TerminalView::new(10, 3);
    for bad in [
        "",
        "not json",
        "{}",
        r#"{"cols":"ten"}"#,
        r#"{"cols":10,"rows":3,"cx":0,"cy":0,"lines":[{"y":"x"}]}"#,
    ] {
        let err = v.apply_snapshot_json(bad).unwrap_err();
        match err {
            CoreError::Protocol { message } => {
                // The message is read by a user, not a developer.
                assert!(
                    message.contains("versions"),
                    "a protocol error should hint at a version mismatch: {message}"
                );
            }
            other => panic!("expected a Protocol error for {bad:?}, got {other:?}"),
        }
    }
}

/// A renderer racing a resize is normal. Crashing the app over a cosmetic race
/// would be absurd.
#[test]
fn out_of_range_row_requests_clamp() {
    let v = TerminalView::new(4, 2);
    assert_eq!(v.rows(0, 100).unwrap().len(), 2, "count clamps to the grid");
    assert_eq!(
        v.rows(99, 10).unwrap().len(),
        0,
        "an out-of-range start is empty, not an error"
    );
    assert_eq!(v.row_text(99).unwrap(), "", "an out-of-range row is empty");
}

/// The three colour cases must be pre-resolved so the app never has to know
/// that -1 means default and a flag bit distinguishes palette from truecolour.
#[test]
fn colours_are_resolved_for_the_renderer() {
    let v = TerminalView::new(3, 1);
    // Cell 0 default, cell 1 palette 208, cell 2 truecolour (10,20,30).
    let rgb = (10 << 16 | 20 << 8 | 30) | (1 << 24);
    v.apply_snapshot_json(&format!(
        r#"{{"cols":3,"rows":1,"cx":0,"cy":0,"lines":[{{"y":0,"r":[
            {{"t":"a","a":{{"FG":-1,"BG":-1}}}},
            {{"t":"b","a":{{"FG":208,"BG":-1}}}},
            {{"t":"c","a":{{"FG":{rgb},"BG":-1}}}}
        ]}}]}}"#
    ))
    .unwrap();

    let row = &v.rows(0, 1).unwrap()[0];
    assert_eq!(row.cells[0].fg_palette, None, "default is not palette 0");
    assert_eq!(row.cells[0].fg_rgb, None, "default is not truecolour");
    assert_eq!(row.cells[1].fg_palette, Some(208));
    assert_eq!(row.cells[1].fg_rgb, None);
    assert_eq!(row.cells[2].fg_rgb, Some(0x0a141e));
    assert_eq!(row.cells[2].fg_palette, None);
}

/// A wide character's continuation column must be skipped by the renderer, not
/// drawn, or every glyph after it shifts.
#[test]
fn wide_continuation_cells_are_marked_and_empty() {
    let v = TerminalView::new(4, 1);
    v.apply_snapshot_json(
        r#"{"cols":4,"rows":1,"cx":0,"cy":0,"lines":[{"y":0,"r":[{"t":"日","a":{"FG":-1,"BG":-1}}]}]}"#,
    )
    .unwrap();
    let row = &v.rows(0, 1).unwrap()[0];
    assert_eq!(row.cells[0].text, "日");
    assert!(row.cells[1].wide_continuation);
    assert_eq!(
        row.cells[1].text, "",
        "a continuation cell must draw nothing"
    );
}

// ---- protocol decoding ----

#[test]
fn decodes_a_real_session_list() {
    // Exactly the shape the daemon's /v1/sessions returns.
    let json = r#"{"sessions":[
      {"id":"s1","agent_id":"claude-code","title":"Refactor billing",
       "cwd":"/repo","git_branch":"main","model":"claude-opus-5",
       "attachment":"native","fidelity":"NATIVE_FULL","state":"WAITING_APPROVAL",
       "prompt_id":"p1",
       "guess":{"state":"WAITING_APPROVAL","confidence":1,"reason":"approval required","evidence":"hook:PermissionRequest"},
       "pending_approval":{"id":"a1","subject":"Bash: rm -rf ./build","options":["allow","deny"]}},
      {"id":"s2","agent_id":"codex","fidelity":"READONLY","state":"RUNNING",
       "guess":{"state":"RUNNING","confidence":0.65,"reason":"rollout changed recently","evidence":"codex:idle 1s"}}
    ]}"#;

    let sessions = decode_sessions(json).unwrap();
    assert_eq!(sessions.len(), 2);

    let a = &sessions[0];
    assert!(a.is_waiting, "an approval state sorts to the top");
    assert!(a.can_input);
    assert!(
        a.is_authoritative,
        "a hook-sourced state is a fact, not a guess"
    );
    assert_eq!(
        a.pending_approval_subject.as_deref(),
        Some("Bash: rm -rf ./build")
    );

    let b = &sessions[1];
    assert!(
        !b.can_input,
        "a READONLY session must not offer a compose bar"
    );
    assert!(
        !b.is_authoritative,
        "an inferred state must be labelled as one"
    );
    assert_eq!(b.state_reason, "rollout changed recently");
}

/// The daemon has returned both an envelope and a bare array. Accepting both
/// means a version bump does not blank the session list.
#[test]
fn accepts_both_session_list_shapes() {
    let bare = r#"[{"id":"s1","agent_id":"pi","fidelity":"FULL","state":"RUNNING"}]"#;
    assert_eq!(decode_sessions(bare).unwrap().len(), 1);
    let wrapped =
        r#"{"sessions":[{"id":"s1","agent_id":"pi","fidelity":"FULL","state":"RUNNING"}]}"#;
    assert_eq!(decode_sessions(wrapped).unwrap().len(), 1);
}

#[test]
fn decodes_a_transcript_page() {
    let json = r#"{"events":[
      {"seq":1,"id":"e1","kind":"user_message","text":"fix the tests"},
      {"seq":2,"id":"e2","parent_id":"e1","kind":"tool_call","tool_name":"Bash"},
      {"seq":3,"id":"e3","parent_id":"e2","kind":"tool_result","tool_output":"2 failing","is_error":true}
    ]}"#;
    let items = decode_transcript(json).unwrap();
    assert_eq!(items.len(), 3);
    assert_eq!(items[1].tool_name, "Bash");
    assert_eq!(items[2].parent_id, "e2", "the transcript tree must survive");
    assert!(items[2].is_error);
}

#[test]
fn malformed_transcript_errors_cleanly() {
    assert!(decode_transcript("garbage").is_err());
    assert!(decode_sessions("garbage").is_err());
}

// ---- pairing ----

/// The human check cannot be skipped: without confirm() there is no session,
/// so nothing can be sent.
#[test]
fn pairing_requires_confirmation_before_use() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use muxscale_core::e2e::{Handshake, KeyPair, Role};

    let daemon = KeyPair::generate().unwrap();
    let pairing = Pairing::new(&STANDARD.encode(&daemon.public)).unwrap();

    let msg1 = STANDARD
        .decode(pairing.first_message_base64().unwrap())
        .unwrap();
    let mut daemon_hs = Handshake::new(Role::Daemon, &daemon, None).unwrap();
    daemon_hs.read_message(&msg1).unwrap();
    let msg2 = daemon_hs.write_message(&[]).unwrap();
    pairing
        .accept_reply_base64(&STANDARD.encode(&msg2))
        .unwrap();

    // Words are available for the human to compare...
    let words = pairing.verification_words().unwrap();
    assert_eq!(words.len(), 8);

    // ...but sending is refused until the human confirms.
    let err = pairing.seal_base64("allow", "hdr").unwrap_err();
    match err {
        CoreError::State { message } => assert!(
            message.contains("confirm"),
            "the refusal should say what is missing: {message}"
        ),
        other => panic!("expected a State error, got {other:?}"),
    }

    pairing.confirm().unwrap();
    assert!(pairing.seal_base64("allow", "hdr").is_ok());
}

#[test]
fn pairing_rejects_a_bad_invite() {
    let err = match Pairing::new("not base64!!!") {
        Ok(_) => panic!("a malformed invite was accepted"),
        Err(e) => e,
    };
    match err {
        CoreError::Crypto { message } => assert!(
            message.contains("QR code"),
            "the message should point at the QR code: {message}"
        ),
        other => panic!("expected a Crypto error, got {other:?}"),
    }
}

#[test]
fn pairing_rejects_a_malformed_reply() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use muxscale_core::e2e::KeyPair;

    let daemon = KeyPair::generate().unwrap();
    let pairing = Pairing::new(&STANDARD.encode(&daemon.public)).unwrap();
    let _ = pairing.first_message_base64().unwrap();

    assert!(pairing.accept_reply_base64("!!!not base64").is_err());
    assert!(
        pairing
            .accept_reply_base64(&STANDARD.encode(b"garbage bytes"))
            .is_err(),
        "a reply that does not verify must be refused"
    );
}

/// Double-confirming, or using a confirmed pairing's handshake, must be a clean
/// error rather than a panic.
#[test]
fn pairing_state_errors_are_clean() {
    use base64::{engine::general_purpose::STANDARD, Engine};
    use muxscale_core::e2e::{Handshake, KeyPair, Role};

    let daemon = KeyPair::generate().unwrap();
    let pairing = Pairing::new(&STANDARD.encode(&daemon.public)).unwrap();
    let msg1 = STANDARD
        .decode(pairing.first_message_base64().unwrap())
        .unwrap();
    let mut daemon_hs = Handshake::new(Role::Daemon, &daemon, None).unwrap();
    daemon_hs.read_message(&msg1).unwrap();
    let msg2 = daemon_hs.write_message(&[]).unwrap();
    pairing
        .accept_reply_base64(&STANDARD.encode(&msg2))
        .unwrap();
    pairing.confirm().unwrap();

    assert!(pairing.confirm().is_err(), "double confirm must error");
    assert!(
        pairing.verification_words().is_err(),
        "words are not available after confirmation"
    );
}

#[test]
fn version_functions_report_something_useful() {
    assert_eq!(protocol_version(), 1);
    assert!(!core_version().is_empty());
}
