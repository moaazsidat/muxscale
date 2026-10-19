// Package kiro adapts AWS's Kiro CLI.
//
// Written from kiro-cli-history 0.4.1 (PyPI, MIT), a session browser that
// reads Kiro CLI's real stores -- not from a running install, and not from
// Kiro's own source, which is not public. `muxscale debug adapter kiro` is the
// check against real files. Two of Kiro's stores are read:
//
//   - CLI 3.0 (current): ~/.kiro/sessions/<workspace-hash>/sess_*/session.json
//     ({id, title, createdAt, lastModifiedAt, workspacePaths}) beside
//     messages.jsonl, where each line is {payload: {type, content}} and type
//     "user" or "assistant" carries the text.
//   - CLI 2.x classic mode: ~/.kiro/sessions/cli/<id>.json ({session_id,
//     title, cwd, created_at, updated_at}) beside <id>.jsonl, each line
//     {kind: "Prompt" | "AssistantMessage", data: {content: [{kind: "text",
//     data}]}}.
//
// What that reader confirms is the conversation's text, so that is all this
// shows: no tool calls, and state inferred from who spoke last. The older
// SQLite stores (data.sqlite3) are not read.
package kiro

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/adapter/emit"
	"github.com/muxscale/muxscale/go/internal/protocol"
)

// MinVersion is nominal: the format was read from a third-party reader.
const MinVersion = "2.0.0"

// meta is a session's metadata file, either generation.
type meta struct {
	// CLI 3.0
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	CreatedAt      string   `json:"createdAt"`
	LastModifiedAt string   `json:"lastModifiedAt"`
	WorkspacePaths []string `json:"workspacePaths"`
	// CLI 2.x
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Created   string `json:"created_at"`
	Updated   string `json:"updated_at"`
}

func (m meta) id() string { return firstNonEmpty(m.ID, m.SessionID) }

func (m meta) cwd() string {
	if len(m.WorkspacePaths) > 0 {
		return m.WorkspacePaths[0]
	}
	return m.Cwd
}

func (m meta) started() time.Time { return parseTime(firstNonEmpty(m.CreatedAt, m.Created)) }

type line struct {
	// CLI 3.0
	Payload *struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	} `json:"payload"`
	// CLI 2.x
	Kind string `json:"kind"`
	Data *struct {
		Content []struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		} `json:"content"`
	} `json:"data"`
}

// Lifecycle names: who spoke last.
const (
	lifeUser      = "kiro:user"
	lifeAssistant = "kiro:assistant"
)

// parse reads a messages file of either generation.
func parse(r io.Reader) ([]emit.Unit, string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256*1024), 64*1024*1024)
	var units []emit.Unit
	life := ""
	n := 0
	for sc.Scan() {
		n++
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		var kind protocol.EventKind
		var text string
		switch {
		case l.Payload != nil && (l.Payload.Type == "user" || l.Payload.Type == "assistant"):
			var s string
			if json.Unmarshal(l.Payload.Content, &s) != nil {
				continue
			}
			text, kind = s, protocol.EventUserMessage
			if l.Payload.Type == "assistant" {
				kind = protocol.EventAssistantMessage
			}
		case l.Kind == "Prompt" || l.Kind == "AssistantMessage":
			if l.Data == nil {
				continue
			}
			for _, c := range l.Data.Content {
				var s string
				if c.Kind == "text" && json.Unmarshal(c.Data, &s) == nil {
					text = s
					break
				}
			}
			kind = protocol.EventUserMessage
			if l.Kind == "AssistantMessage" {
				kind = protocol.EventAssistantMessage
			}
		default:
			continue
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		key := "l" + strconv.Itoa(n)
		ev := protocol.TranscriptEvent{ID: key, Kind: kind}
		ev.Text, ev.Truncated = clip(strings.TrimSpace(text))
		units = append(units, emit.Unit{Key: key, Events: []protocol.TranscriptEvent{ev}, Final: true})
		life = lifeUser
		if kind == protocol.EventAssistantMessage {
			life = lifeAssistant
		}
	}
	return units, life, sc.Err()
}

func stateFromLifecycle(life string, idle, quiescence time.Duration) (protocol.StateGuess, bool) {
	now := time.Now()
	g := func(s protocol.SessionState, c float64, reason string) (protocol.StateGuess, bool) {
		return protocol.StateGuess{State: s, Confidence: c, Reason: reason, Evidence: life, At: now}, true
	}
	switch life {
	case lifeAssistant:
		if idle >= quiescence {
			// Kiro's own record of tools and turn ends is not read, so this
			// stays a guess: a quiet session whose last word was Kiro's.
			return g(protocol.StateWaitingInput, 0.6, "Kiro spoke last and the session has been quiet")
		}
		return g(protocol.StateRunning, 0.55, "Kiro is answering")
	case lifeUser:
		return g(protocol.StateRunning, 0.5, "a prompt is waiting for an answer")
	}
	return protocol.StateGuess{}, false
}

// ---- paths ----

func sessionsRoot() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".kiro", "sessions"), nil
}

// found is one session on disk.
type found struct {
	meta     meta
	messages string
	mod      time.Time
}

// list walks both generations' layouts.
func list() ([]found, error) {
	root, err := sessionsRoot()
	if err != nil {
		return nil, err
	}
	var out []found
	add := func(metaPath, messages string) {
		raw, err := os.ReadFile(metaPath)
		if err != nil {
			return
		}
		var m meta
		if json.Unmarshal(raw, &m) != nil || m.id() == "" {
			return
		}
		f := found{meta: m, messages: messages}
		if fi, err := os.Stat(messages); err == nil {
			f.mod = fi.ModTime()
		} else if fi, err := os.Stat(metaPath); err == nil {
			f.mod = fi.ModTime()
		}
		out = append(out, f)
	}
	v3, _ := filepath.Glob(filepath.Join(root, "*", "sess_*", "session.json"))
	for _, p := range v3 {
		add(p, filepath.Join(filepath.Dir(p), "messages.jsonl"))
	}
	v2, _ := filepath.Glob(filepath.Join(root, "cli", "*.json"))
	for _, p := range v2 {
		add(p, strings.TrimSuffix(p, ".json")+".jsonl")
	}
	return out, nil
}

// ---- helpers ----

const excerptBytes = 4096

func clip(s string) (string, bool) {
	if len(s) <= excerptBytes {
		return s, false
	}
	cut := excerptBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut], true
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
