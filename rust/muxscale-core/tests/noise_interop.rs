//! Live Noise interop against the real Go daemon implementation.
//!
//! Two implementations of a security protocol that only ever test against
//! themselves will agree with themselves. A disagreement between them does not
//! fail loudly: it produces a pairing that appears to work while the SAS check
//! silently compares two different phrases, so a legitimate pairing is
//! abandoned or -- worse, depending which way it breaks -- a bad one is not
//! caught.
//!
//! So this spawns `go/cmd/noise-peer`, performs an actual Noise IK handshake
//! against it, compares the short authentication string both sides derived
//! independently, and exchanges a sealed message in both directions.
//!
//! Skipped when the Go peer has not been built, so `cargo test` alone still
//! works. Build it with:
//!   cd go && go build -o /tmp/noise-peer ./cmd/noise-peer

use base64::{engine::general_purpose::STANDARD as B64, Engine};
use muxscale_core::e2e::{sas, Handshake, KeyPair, Role};
use serde::{Deserialize, Serialize};
use std::io::{BufRead, BufReader, Write};
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};

#[derive(Debug, Default, Serialize, Deserialize)]
struct Msg {
    #[serde(rename = "type")]
    kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    data: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    header: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    daemon_public: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    peer_public: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    plaintext: String,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    sas: Vec<String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    message: String,
}

struct Peer {
    child: Child,
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
}

impl Peer {
    fn spawn() -> Option<Self> {
        let path = "/tmp/noise-peer";
        if !std::path::Path::new(path).exists() {
            return None;
        }
        let mut child = Command::new(path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .ok()?;
        let stdin = child.stdin.take()?;
        let stdout = BufReader::new(child.stdout.take()?);
        Some(Peer {
            child,
            stdin,
            stdout,
        })
    }

    fn send(&mut self, m: &Msg) {
        let line = serde_json::to_string(m).unwrap();
        writeln!(self.stdin, "{line}").expect("writing to the Go peer");
        self.stdin.flush().unwrap();
    }

    fn recv(&mut self) -> Msg {
        let mut line = String::new();
        self.stdout
            .read_line(&mut line)
            .expect("reading from the Go peer");
        assert!(!line.trim().is_empty(), "the Go peer closed unexpectedly");
        serde_json::from_str(&line).unwrap_or_else(|e| panic!("bad reply {line:?}: {e}"))
    }
}

impl Drop for Peer {
    fn drop(&mut self) {
        let _ = self.send_bye();
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

impl Peer {
    fn send_bye(&mut self) -> std::io::Result<()> {
        writeln!(self.stdin, r#"{{"type":"bye"}}"#)
    }
}

fn b64(b: &[u8]) -> String {
    B64.encode(b)
}

fn unb64(s: &str) -> Vec<u8> {
    B64.decode(s).expect("bad base64 from the Go peer")
}

/// The whole point: a real handshake between the two implementations, with
/// both sides deriving the SAS independently.
#[test]
fn go_and_rust_complete_a_handshake_and_agree_on_the_sas() {
    let Some(mut peer) = Peer::spawn() else {
        eprintln!(
            "skipping: build the Go peer with `go build -o /tmp/noise-peer ./cmd/noise-peer`"
        );
        return;
    };

    let hello = peer.recv();
    assert_eq!(hello.kind, "hello");
    let daemon_public = unb64(&hello.daemon_public);
    assert_eq!(daemon_public.len(), 32);

    // Rust plays the device, exactly as a phone that scanned the QR code would.
    let device = KeyPair::generate().unwrap();
    let mut hs = Handshake::new(Role::Device, &device, Some(&daemon_public)).unwrap();

    let msg1 = hs.write_message(&[]).unwrap();
    peer.send(&Msg {
        kind: "msg1".into(),
        data: b64(&msg1),
        ..Default::default()
    });

    let reply = peer.recv();
    assert_eq!(reply.kind, "msg2", "Go rejected our handshake: {reply:?}");
    hs.read_message(&unb64(&reply.data))
        .expect("Go's handshake reply did not verify");
    assert!(hs.complete(), "the handshake did not complete");

    // The daemon must have learned our key, which is what it records when
    // enrolling the device.
    assert_eq!(
        unb64(&reply.peer_public),
        device.public,
        "Go learned the wrong device key"
    );

    // The value everything rests on: both sides derived the same phrase from
    // their own view of the handshake.
    let rust_words: Vec<String> = sas(hs.channel_binding())
        .unwrap()
        .into_iter()
        .map(String::from)
        .collect();
    assert_eq!(
        rust_words, reply.sas,
        "Go and Rust derived different short authentication strings.\n\
         This is the failure that matters: two honest parties would show \
         different phrases and abandon a legitimate pairing.\n\
         go:   {:?}\n  rust: {:?}",
        reply.sas, rust_words
    );
    println!("both sides agree on SAS: {}", rust_words.join(" "));

    // And a sealed message crosses correctly.
    let mut session = hs.into_session().unwrap();
    let header = b"v1|hk_test|dev_rust|1";
    let secret = b"approve the rm -rf";
    let sealed = session.seal(secret, header).unwrap();

    peer.send(&Msg {
        kind: "sealed".into(),
        data: b64(&sealed),
        header: b64(header),
        ..Default::default()
    });
    let opened = peer.recv();
    assert_eq!(
        opened.kind, "opened",
        "Go could not open a message Rust sealed: {}",
        opened.message
    );
    assert_eq!(
        unb64(&opened.plaintext),
        secret,
        "the payload changed in transit"
    );
}

/// A relay that rewrites the routing header must be caught by the other
/// implementation, not just by our own.
#[test]
fn go_refuses_a_message_rust_addressed_elsewhere() {
    let Some(mut peer) = Peer::spawn() else {
        eprintln!("skipping: Go peer not built");
        return;
    };

    let hello = peer.recv();
    let daemon_public = unb64(&hello.daemon_public);
    let device = KeyPair::generate().unwrap();
    let mut hs = Handshake::new(Role::Device, &device, Some(&daemon_public)).unwrap();

    let msg1 = hs.write_message(&[]).unwrap();
    peer.send(&Msg {
        kind: "msg1".into(),
        data: b64(&msg1),
        ..Default::default()
    });
    let reply = peer.recv();
    hs.read_message(&unb64(&reply.data)).unwrap();
    let mut session = hs.into_session().unwrap();

    // Seal for one device, deliver claiming another -- what a malicious relay
    // would do.
    let sealed = session.seal(b"allow", b"v1|hk|dev_honest|1").unwrap();
    peer.send(&Msg {
        kind: "sealed".into(),
        data: b64(&sealed),
        header: b64(b"v1|hk|dev_attacker|1"),
        ..Default::default()
    });

    let result = peer.recv();
    assert_eq!(
        result.kind, "error",
        "Go accepted a redirected message; the header binding does not work across implementations"
    );
    assert!(
        result.message.contains("redirected") || result.message.contains("decryption failed"),
        "the refusal should explain itself: {}",
        result.message
    );
}

/// A device that does not have the daemon's real key cannot complete IK. This
/// is what makes the QR code the root of trust, and it has to hold against the
/// other implementation too.
#[test]
fn go_rejects_a_handshake_addressed_to_the_wrong_key() {
    let Some(mut peer) = Peer::spawn() else {
        eprintln!("skipping: Go peer not built");
        return;
    };

    let _hello = peer.recv();
    let imposter = KeyPair::generate().unwrap();
    let device = KeyPair::generate().unwrap();

    // Handshake addressed to a key the daemon does not hold.
    let mut hs = Handshake::new(Role::Device, &device, Some(&imposter.public)).unwrap();
    let msg1 = hs.write_message(&[]).unwrap();
    peer.send(&Msg {
        kind: "msg1".into(),
        data: b64(&msg1),
        ..Default::default()
    });

    let reply = peer.recv();
    assert_eq!(
        reply.kind, "error",
        "Go accepted a handshake addressed to a different static key"
    );
}
