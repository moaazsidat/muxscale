//! End-to-end encryption, mirroring `go/internal/e2e`.
//!
//! This is the second implementation of a security protocol, which is a thing
//! to be nervous about: a subtle disagreement between the two sides does not
//! produce a clear failure, it produces a pairing that appears to work while
//! the SAS check silently compares two different things. So
//! `tests/noise_interop.rs` runs a real handshake against the Go daemon over a
//! pipe rather than asserting against recorded values.
//!
//! Noise IK over X25519 + ChaCha20-Poly1305 + BLAKE2s. IK because the device
//! already has the daemon's static key from the QR code, so the daemon is
//! authenticated on the first message and the device's key travels encrypted.

use blake2::{Blake2s256, Digest};
use snow::{Builder, HandshakeState, TransportState};

/// The Noise pattern string. Must match Go exactly; a mismatch fails the
/// handshake outright, which is the good failure mode.
pub const NOISE_PARAMS: &str = "Noise_IK_25519_ChaChaPoly_BLAKE2s";

/// Binds the handshake to this application and version, so two muxscale
/// versions that disagree about the protocol fail to connect rather than
/// negotiating something neither intended.
pub const PROLOGUE: &[u8] = b"muxscale/1 device-pairing";

/// Errors from the transport.
#[derive(Debug)]
pub enum Error {
    /// The handshake or a message failed cryptographic verification. This
    /// covers tampering, replay, and out-of-order delivery alike, because from
    /// here they are indistinguishable and all mean "do not trust this".
    Crypto(String),
    /// A key was the wrong length or shape.
    BadKey(String),
    /// An operation was attempted before the handshake finished.
    NotReady,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::Crypto(m) => write!(
                f,
                "decryption failed (tampered, replayed, or out of order): {m}"
            ),
            Error::BadKey(m) => write!(f, "bad key: {m}"),
            Error::NotReady => write!(f, "handshake is not complete"),
        }
    }
}

impl std::error::Error for Error {}

fn crypto<E: std::fmt::Display>(e: E) -> Error {
    Error::Crypto(e.to_string())
}

/// An X25519 identity.
#[derive(Clone)]
pub struct KeyPair {
    pub public: Vec<u8>,
    pub private: Vec<u8>,
}

impl KeyPair {
    /// Generates a new identity.
    ///
    /// On a real device this belongs in the secure enclave and never reaches
    /// Rust; this is for the daemon side, tests, and platforms without hardware
    /// key storage.
    pub fn generate() -> Result<Self, Error> {
        let kp = Builder::new(NOISE_PARAMS.parse().map_err(crypto)?)
            .generate_keypair()
            .map_err(crypto)?;
        Ok(KeyPair {
            public: kp.public,
            private: kp.private,
        })
    }

    /// Rebuilds a key pair from stored bytes.
    pub fn from_parts(public: Vec<u8>, private: Vec<u8>) -> Result<Self, Error> {
        if public.len() != 32 || private.len() != 32 {
            return Err(Error::BadKey(format!(
                "expected 32-byte keys, got {} and {}",
                public.len(),
                private.len()
            )));
        }
        Ok(KeyPair { public, private })
    }
}

/// Which side of the handshake this party is on.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Role {
    /// The host being connected to: the responder.
    Daemon,
    /// The phone connecting in: the initiator.
    Device,
}

/// A Noise IK handshake in progress.
pub struct Handshake {
    state: Option<HandshakeState>,
    transport: Option<TransportState>,
    binding: Vec<u8>,
    peer_static: Option<Vec<u8>>,
}

impl Handshake {
    /// Starts a handshake.
    ///
    /// `remote_static` is required for the device -- it knows the daemon's key
    /// from the QR code -- and ignored for the daemon, which learns the
    /// device's key during the exchange.
    pub fn new(role: Role, local: &KeyPair, remote_static: Option<&[u8]>) -> Result<Self, Error> {
        let params = NOISE_PARAMS.parse().map_err(crypto)?;
        let mut builder = Builder::new(params)
            .prologue(PROLOGUE)
            .map_err(crypto)?
            .local_private_key(&local.private)
            .map_err(crypto)?;

        let state = match role {
            Role::Device => {
                let rs = remote_static.ok_or_else(|| {
                    Error::BadKey(
                        "the device must know the daemon's public key before handshaking".into(),
                    )
                })?;
                if rs.len() != 32 {
                    return Err(Error::BadKey(format!(
                        "daemon public key is {} bytes, want 32",
                        rs.len()
                    )));
                }
                builder = builder.remote_public_key(rs).map_err(crypto)?;
                builder.build_initiator().map_err(crypto)?
            }
            Role::Daemon => builder.build_responder().map_err(crypto)?,
        };

        Ok(Handshake {
            state: Some(state),
            transport: None,
            binding: Vec::new(),
            peer_static: None,
        })
    }

    /// Produces the next handshake message.
    pub fn write_message(&mut self, payload: &[u8]) -> Result<Vec<u8>, Error> {
        let state = self.state.as_mut().ok_or(Error::NotReady)?;
        let mut buf = vec![0u8; 65535];
        let n = state.write_message(payload, &mut buf).map_err(crypto)?;
        buf.truncate(n);
        self.maybe_finish()?;
        Ok(buf)
    }

    /// Consumes a handshake message, returning its payload.
    pub fn read_message(&mut self, msg: &[u8]) -> Result<Vec<u8>, Error> {
        let state = self.state.as_mut().ok_or(Error::NotReady)?;
        let mut buf = vec![0u8; 65535];
        let n = state.read_message(msg, &mut buf).map_err(crypto)?;
        buf.truncate(n);
        self.maybe_finish()?;
        Ok(buf)
    }

    /// Captures the binding and peer key, then promotes to transport mode once
    /// the handshake completes.
    fn maybe_finish(&mut self) -> Result<(), Error> {
        let done = self
            .state
            .as_ref()
            .map(|s| s.is_handshake_finished())
            .unwrap_or(false);
        if !done {
            return Ok(());
        }
        let state = self.state.take().ok_or(Error::NotReady)?;

        // The binding and peer key must be read before the state is consumed.
        self.binding = state.get_handshake_hash().to_vec();
        self.peer_static = state.get_remote_static().map(|k| k.to_vec());

        self.transport = Some(state.into_transport_mode().map_err(crypto)?);
        Ok(())
    }

    /// Whether the handshake has finished.
    pub fn complete(&self) -> bool {
        self.transport.is_some()
    }

    /// The other side's public key, known once the handshake completes.
    pub fn peer_static(&self) -> Option<&[u8]> {
        self.peer_static.as_deref()
    }

    /// The handshake hash the short authentication string is derived from.
    ///
    /// It commits to both parties' keys and every handshake message, so two
    /// sessions with a relay in the middle cannot produce the same binding.
    pub fn channel_binding(&self) -> &[u8] {
        &self.binding
    }

    /// Takes the established encrypted channel.
    pub fn into_session(self) -> Result<Session, Error> {
        let transport = self.transport.ok_or(Error::NotReady)?;
        Ok(Session { transport })
    }
}

/// Bounds the framed routing header, so a malformed length cannot make `open`
/// allocate wildly.
pub const MAX_HEADER_BYTES: usize = 4096;

/// An established encrypted channel.
pub struct Session {
    transport: TransportState,
}

impl Session {
    /// Encrypts a payload for the peer, binding it to a routing header.
    ///
    /// The header is prepended to the plaintext rather than passed as Noise
    /// additional-authenticated-data. The Noise spec says transport messages
    /// carry no AD, and snow implements that strictly, so AD was simply not
    /// available -- which is how the Go side's original design was found to be
    /// unimplementable in a compliant library. A protocol only one library can
    /// speak is not a protocol.
    ///
    /// The security property is unchanged: a relay that rewrites the outer
    /// routing header produces a mismatch against this authenticated inner
    /// copy, and `open` rejects it.
    pub fn seal(&mut self, plaintext: &[u8], header: &[u8]) -> Result<Vec<u8>, Error> {
        if header.len() > MAX_HEADER_BYTES {
            return Err(Error::BadKey(format!(
                "routing header is {} bytes, over the {MAX_HEADER_BYTES} limit",
                header.len()
            )));
        }
        let mut framed = Vec::with_capacity(2 + header.len() + plaintext.len());
        framed.extend_from_slice(&(header.len() as u16).to_be_bytes());
        framed.extend_from_slice(header);
        framed.extend_from_slice(plaintext);

        let mut buf = vec![0u8; framed.len() + 64];
        let n = self
            .transport
            .write_message(&framed, &mut buf)
            .map_err(crypto)?;
        buf.truncate(n);
        Ok(buf)
    }

    /// Decrypts a payload and verifies it was addressed as received.
    ///
    /// `want_header` is the routing header the message actually arrived under.
    /// If the authenticated copy inside disagrees, the message was redirected in
    /// transit and is refused.
    ///
    /// Noise's nonce sequence provides replay protection: a repeated or
    /// reordered message fails rather than being accepted twice. That matters
    /// more than usual here, because a replayed message could be an approval.
    pub fn open(&mut self, ciphertext: &[u8], want_header: &[u8]) -> Result<Vec<u8>, Error> {
        let mut buf = vec![0u8; ciphertext.len() + 64];
        let n = self
            .transport
            .read_message(ciphertext, &mut buf)
            .map_err(crypto)?;
        buf.truncate(n);

        if buf.len() < 2 {
            return Err(Error::Crypto(
                "message is too short to carry a routing header".into(),
            ));
        }
        let hlen = u16::from_be_bytes([buf[0], buf[1]]) as usize;
        if hlen > MAX_HEADER_BYTES || 2 + hlen > buf.len() {
            return Err(Error::Crypto(
                "routing header length is out of range".into(),
            ));
        }
        if &buf[2..2 + hlen] != want_header {
            return Err(Error::Crypto(format!(
                "message was addressed to {:?} but delivered as {:?}; it was redirected in transit",
                String::from_utf8_lossy(&buf[2..2 + hlen]),
                String::from_utf8_lossy(want_header)
            )));
        }
        Ok(buf[2 + hlen..].to_vec())
    }
}

/// How many words the short authentication string uses.
pub const SAS_WORD_COUNT: usize = 8;

/// The SAS word list. Must be byte-identical to the Go list, in the same order:
/// the words are selected by index, so any divergence makes two honest parties
/// display different phrases and abandon a legitimate pairing.
pub const SAS_WORDS: [&str; 256] = [
    "acid", "acorn", "alarm", "album", "amber", "ankle", "apple", "apron", "arrow", "atlas",
    "aunt", "award", "bacon", "badge", "baker", "banjo", "basil", "beach", "beard", "began",
    "belly", "bench", "berry", "bison", "blade", "blank", "blaze", "blend", "blimp", "block",
    "blush", "board", "bonus", "boost", "botch", "brace", "brain", "brass", "bread", "brick",
    "brief", "bring", "brisk", "broke", "brush", "buddy", "buggy", "build", "bunch", "bunny",
    "cabin", "cable", "cache", "camel", "candy", "canoe", "cargo", "carve", "catch", "cause",
    "chalk", "charm", "chase", "check", "chess", "chief", "chime", "choir", "chunk", "cider",
    "cigar", "civic", "claim", "clamp", "clash", "clean", "clerk", "cliff", "climb", "cloak",
    "clock", "cloud", "clove", "clown", "coach", "coast", "cobra", "cocoa", "comet", "coral",
    "couch", "cough", "cover", "crane", "crash", "crate", "crawl", "creek", "crisp", "crown",
    "crumb", "crush", "cubic", "curve", "cycle", "daisy", "dance", "dandy", "debug", "decoy",
    "delta", "dense", "depot", "devil", "diary", "diner", "dingo", "ditch", "diver", "dizzy",
    "dodge", "donor", "donut", "draft", "drama", "dream", "dress", "drift", "drill", "drink",
    "drive", "drone", "drown", "dwarf", "eagle", "early", "earth", "easel", "eaten", "ebony",
    "eight", "elbow", "elder", "elfin", "elite", "elope", "ember", "empty", "enemy", "enjoy",
    "entry", "equal", "erase", "error", "essay", "ether", "event", "every", "exact", "exile",
    "extra", "fable", "faint", "fairy", "faith", "false", "fancy", "fatal", "fault", "favor",
    "feast", "fence", "ferry", "fetch", "fever", "fiber", "field", "fiery", "fifth", "fifty",
    "fight", "final", "finch", "first", "flame", "flash", "fleet", "flesh", "flint", "float",
    "flock", "flood", "floor", "flour", "fluid", "flute", "foamy", "focal", "focus", "foggy",
    "force", "forge", "forty", "found", "frame", "fraud", "fresh", "fried", "frost", "fruit",
    "fudge", "fuzzy", "gauge", "gecko", "ghost", "giant", "giddy", "girth", "given", "glade",
    "gland", "glare", "glass", "glaze", "gleam", "globe", "gloom", "glory", "glove", "gnome",
    "goose", "grace", "grain", "grand", "grape", "graph", "grasp", "grass", "grave", "gravy",
    "graze", "great", "greed", "green", "greet", "grief", "grill", "grime", "grind", "gripe",
    "groan", "groom", "grove", "growl", "grunt", "guard",
];

/// Derives the short authentication string from a handshake's channel binding.
///
/// Both sides compute this independently. A relay in the middle necessarily
/// runs two different handshakes, so it cannot make both sides display the same
/// words -- which is exactly what the human comparison detects.
///
/// The domain-separation prefix and the BLAKE2s construction must match Go
/// byte-for-byte, or two honest parties disagree and abandon a good pairing.
pub fn sas(channel_binding: &[u8]) -> Result<Vec<&'static str>, Error> {
    if channel_binding.is_empty() {
        return Err(Error::NotReady);
    }
    let mut h = Blake2s256::new();
    h.update(b"muxscale-sas-v1\x00");
    h.update(channel_binding);
    let sum = h.finalize();

    Ok((0..SAS_WORD_COUNT)
        .map(|i| SAS_WORDS[sum[i] as usize])
        .collect())
}

/// Renders the words for display.
pub fn sas_string(words: &[&str]) -> String {
    words.join(" ")
}

#[cfg(test)]
mod tests {
    use super::*;

    fn handshake() -> (Handshake, Handshake) {
        let daemon = KeyPair::generate().unwrap();
        let device = KeyPair::generate().unwrap();

        let mut d = Handshake::new(Role::Device, &device, Some(&daemon.public)).unwrap();
        let mut h = Handshake::new(Role::Daemon, &daemon, None).unwrap();

        let m1 = d.write_message(&[]).unwrap();
        h.read_message(&m1).unwrap();
        let m2 = h.write_message(&[]).unwrap();
        d.read_message(&m2).unwrap();

        assert!(d.complete() && h.complete());
        (h, d)
    }

    #[test]
    fn round_trip() {
        let (daemon_hs, device_hs) = handshake();
        let mut daemon = daemon_hs.into_session().unwrap();
        let mut device = device_hs.into_session().unwrap();

        let ad = b"v1|hk_1|dev_1|1";
        let ct = device.seal(b"approve the rm -rf", ad).unwrap();
        let pt = daemon.open(&ct, ad).unwrap();
        assert_eq!(pt, b"approve the rm -rf");
    }

    #[test]
    fn word_list_is_exactly_a_power_of_two() {
        assert_eq!(
            SAS_WORDS.len(),
            256,
            "a non-power-of-two list biases early words"
        );
        let mut sorted: Vec<&str> = SAS_WORDS.to_vec();
        sorted.sort_unstable();
        sorted.dedup();
        assert_eq!(
            sorted.len(),
            256,
            "duplicate words reduce the effective entropy"
        );
    }

    #[test]
    fn sas_matches_between_both_sides() {
        let (daemon_hs, device_hs) = handshake();
        let a = sas(daemon_hs.channel_binding()).unwrap();
        let b = sas(device_hs.channel_binding()).unwrap();
        assert_eq!(
            a, b,
            "both sides of a genuine pairing must show the same words"
        );
        assert_eq!(a.len(), SAS_WORD_COUNT);
    }

    /// A relay that terminates one handshake and originates another: both
    /// halves succeed cryptographically, and the words differ.
    #[test]
    fn sas_differs_under_a_relay_in_the_middle() {
        let daemon = KeyPair::generate().unwrap();
        let device = KeyPair::generate().unwrap();
        let relay = KeyPair::generate().unwrap();

        let mut device_hs = Handshake::new(Role::Device, &device, Some(&relay.public)).unwrap();
        let mut relay_as_daemon = Handshake::new(Role::Daemon, &relay, None).unwrap();
        let m1 = device_hs.write_message(&[]).unwrap();
        relay_as_daemon.read_message(&m1).unwrap();
        let m2 = relay_as_daemon.write_message(&[]).unwrap();
        device_hs.read_message(&m2).unwrap();

        let mut relay_as_device =
            Handshake::new(Role::Device, &relay, Some(&daemon.public)).unwrap();
        let mut daemon_hs = Handshake::new(Role::Daemon, &daemon, None).unwrap();
        let m3 = relay_as_device.write_message(&[]).unwrap();
        daemon_hs.read_message(&m3).unwrap();
        let m4 = daemon_hs.write_message(&[]).unwrap();
        relay_as_device.read_message(&m4).unwrap();

        let device_words = sas(device_hs.channel_binding()).unwrap();
        let daemon_words = sas(daemon_hs.channel_binding()).unwrap();
        assert_ne!(
            device_words, daemon_words,
            "a relay in the middle produced matching words; the SAS check is useless"
        );
    }

    #[test]
    fn tampered_ciphertext_fails() {
        let (daemon_hs, device_hs) = handshake();
        let mut daemon = daemon_hs.into_session().unwrap();
        let mut device = device_hs.into_session().unwrap();

        let ad = b"hdr";
        let mut ct = device.seal(b"allow", ad).unwrap();
        let mid = ct.len() / 2;
        ct[mid] ^= 0xff;
        assert!(daemon.open(&ct, ad).is_err());
    }

    /// A redirected message must be detected: the routing header is bound
    /// into the authenticated plaintext.
    #[test]
    fn redirected_message_is_refused() {
        let (daemon_hs, device_hs) = handshake();
        let mut daemon = daemon_hs.into_session().unwrap();
        let mut device = device_hs.into_session().unwrap();

        let ct = device.seal(b"allow", b"v1|hk|dev_honest|1").unwrap();
        assert!(daemon.open(&ct, b"v1|hk|dev_attacker|1").is_err());
    }

    /// A replayed message could be a replayed approval.
    #[test]
    fn replay_is_rejected() {
        let (daemon_hs, device_hs) = handshake();
        let mut daemon = daemon_hs.into_session().unwrap();
        let mut device = device_hs.into_session().unwrap();

        let ad = b"hdr";
        let ct = device.seal(b"allow", ad).unwrap();
        assert!(daemon.open(&ct, ad).is_ok());
        assert!(
            daemon.open(&ct, ad).is_err(),
            "a replayed approval was accepted"
        );
    }

    #[test]
    fn device_must_know_the_daemon_key() {
        let kp = KeyPair::generate().unwrap();
        assert!(Handshake::new(Role::Device, &kp, None).is_err());
        assert!(Handshake::new(Role::Device, &kp, Some(b"short")).is_err());
    }

    #[test]
    fn daemon_learns_the_device_key() {
        let daemon = KeyPair::generate().unwrap();
        let device = KeyPair::generate().unwrap();
        let mut d = Handshake::new(Role::Device, &device, Some(&daemon.public)).unwrap();
        let mut h = Handshake::new(Role::Daemon, &daemon, None).unwrap();
        let m1 = d.write_message(&[]).unwrap();
        h.read_message(&m1).unwrap();
        let m2 = h.write_message(&[]).unwrap();
        d.read_message(&m2).unwrap();

        assert_eq!(
            h.peer_static().unwrap(),
            &device.public[..],
            "the daemon must learn which device it paired with"
        );
    }

    #[test]
    fn wrong_daemon_key_fails_the_handshake() {
        let daemon = KeyPair::generate().unwrap();
        let device = KeyPair::generate().unwrap();
        let imposter = KeyPair::generate().unwrap();

        let mut d = Handshake::new(Role::Device, &device, Some(&imposter.public)).unwrap();
        let mut h = Handshake::new(Role::Daemon, &daemon, None).unwrap();
        let m1 = d.write_message(&[]).unwrap();
        assert!(
            h.read_message(&m1).is_err(),
            "the daemon accepted a handshake addressed to a different key"
        );
    }
}
