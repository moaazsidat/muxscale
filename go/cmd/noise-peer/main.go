// Command noise-peer is a test helper: it plays the daemon side of a muxscale
// pairing handshake over stdin/stdout so the Rust client core can be tested
// against the real Go implementation rather than against recorded values.
//
// Two implementations of a security protocol that only ever test against
// themselves will agree with themselves. A disagreement between them does not
// fail loudly -- it produces a pairing that appears to work while the SAS check
// silently compares two different things. This binary exists so that cannot
// happen unnoticed.
//
// Protocol on the wire (one JSON object per line, both directions):
//
//	<- {"type":"hello","daemon_public":"<b64>"}
//	-> {"type":"msg1","data":"<b64>"}
//	<- {"type":"msg2","data":"<b64>","sas":["..."],"peer_public":"<b64>"}
//	-> {"type":"sealed","data":"<b64>","header":"<b64>"}
//	<- {"type":"opened","plaintext":"<b64>"}  or  {"type":"error","message":"..."}
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/muxscale/muxscale/go/internal/e2e"
)

type msg struct {
	Type         string   `json:"type"`
	Data         string   `json:"data,omitempty"`
	Header       string   `json:"header,omitempty"`
	DaemonPublic string   `json:"daemon_public,omitempty"`
	PeerPublic   string   `json:"peer_public,omitempty"`
	Plaintext    string   `json:"plaintext,omitempty"`
	SAS          []string `json:"sas,omitempty"`
	Message      string   `json:"message,omitempty"`
}

func main() {
	if err := run(); err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(msg{Type: "error", Message: err.Error()})
		os.Exit(1)
	}
}

func run() error {
	kp, err := e2e.GenerateKeyPair()
	if err != nil {
		return err
	}
	hs, err := e2e.NewHandshake(e2e.RoleDaemon, kp, nil)
	if err != nil {
		return err
	}

	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	// Tell the client our static key, as a QR code would.
	if err := out.Encode(msg{Type: "hello", DaemonPublic: b64(kp.Public)}); err != nil {
		return err
	}

	var sess *e2e.Session
	for in.Scan() {
		var m msg
		if err := json.Unmarshal(in.Bytes(), &m); err != nil {
			return fmt.Errorf("bad input: %w", err)
		}

		switch m.Type {
		case "msg1":
			raw, err := unb64(m.Data)
			if err != nil {
				return err
			}
			if _, err := hs.ReadMessage(raw); err != nil {
				return fmt.Errorf("reading msg1: %w", err)
			}
			reply, err := hs.WriteMessage(nil)
			if err != nil {
				return fmt.Errorf("writing msg2: %w", err)
			}
			words, err := e2e.SAS(hs.ChannelBinding())
			if err != nil {
				return err
			}
			if sess, err = hs.Session(); err != nil {
				return err
			}
			if err := out.Encode(msg{
				Type: "msg2", Data: b64(reply), SAS: words,
				PeerPublic: b64(hs.PeerStatic()),
			}); err != nil {
				return err
			}

		case "sealed":
			if sess == nil {
				return fmt.Errorf("sealed message before the handshake completed")
			}
			ct, err := unb64(m.Data)
			if err != nil {
				return err
			}
			hdr, err := unb64(m.Header)
			if err != nil {
				return err
			}
			pt, err := sess.Open(ct, hdr)
			if err != nil {
				if err := out.Encode(msg{Type: "error", Message: err.Error()}); err != nil {
					return err
				}
				continue
			}
			if err := out.Encode(msg{Type: "opened", Plaintext: b64(pt)}); err != nil {
				return err
			}

		case "bye":
			return nil
		}
	}
	return in.Err()
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
