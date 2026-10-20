package opencode

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/muxscale/muxscale/go/internal/protocol"
)

// OpenCode persists the prompts a person sends while a session is busy, each
// with how it will be delivered: "queue" (after the current work) or "steer"
// (at the next step). Two generations of the table, read from OpenCode's own
// migrations:
//
//   - session_inbox (current, 2026-08 on): one row per pending item, deleted
//     when the session takes it; type "user" rows carry a Prompt payload
//     ({text, files?, …}) as JSON.
//   - session_input (1.18, before that migration dropped it): rows stay, and
//     promoted_seq is set once taken; prompt is the payload.
//
// Rows of the other inbox types (synthetic, compaction, move) are OpenCode's
// own work, not something a person typed, and are not shown.
//
// 1.18 creates session_input but does not use it (verified against 1.18.33: a
// prompt typed while busy left it empty). There a queued prompt is an ordinary
// user message, created after the assistant reply still in progress and not
// yet answered by any assistant message -- the rule OpenCode's own TUI uses
// for its QUEUED badge. readQueue falls back to that rule, and so reads every
// version: the inbox when it exists, else pending session_input rows, else the
// messages.

// queueTable names the table this database keeps its queue in, "" for none.
func queueTable(db *sql.DB) string {
	for _, name := range []string{"session_inbox", "session_input"} {
		var n int
		if db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n) == nil && n > 0 {
			return name
		}
	}
	return ""
}

// readQueue lists a session's pending prompts, oldest first.
func readQueue(db *sql.DB, table, sessionID string) ([]protocol.QueuedMessage, error) {
	var q string
	switch table {
	case "session_inbox":
		q = `SELECT payload, delivery, time_created FROM session_inbox
		     WHERE session_id = ? AND type = 'user' ORDER BY enqueued_seq`
	case "session_input":
		q = `SELECT prompt, delivery, time_created FROM session_input
		     WHERE session_id = ? AND promoted_seq IS NULL ORDER BY admitted_seq`
	default:
		return []protocol.QueuedMessage{}, nil
	}
	rows, err := db.Query(q, sessionID)
	if err != nil {
		return nil, err
	}
	out := []protocol.QueuedMessage{}
	for rows.Next() {
		var payload, delivery string
		var created int64
		if err := rows.Scan(&payload, &delivery, &created); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, protocol.QueuedMessage{
			Text:     promptText(payload),
			Delivery: delivery,
			At:       time.UnixMilli(created),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if table == "session_input" && len(out) == 0 {
		return queuedMessages(db, sessionID)
	}
	return out, nil
}

// queuedMessages applies the TUI's rule to the messages themselves: user
// messages after the unfinished assistant reply that nothing has answered.
func queuedMessages(db *sql.DB, sessionID string) ([]protocol.QueuedMessage, error) {
	rows, err := db.Query(`SELECT id, time_created, data FROM message WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	type msg struct {
		id      string
		created int64
		m       storedMessage
	}
	var all []msg
	answered := map[string]bool{}
	var pending *msg
	for rows.Next() {
		var x msg
		var data string
		if err := rows.Scan(&x.id, &x.created, &data); err != nil {
			rows.Close()
			return nil, err
		}
		if json.Unmarshal([]byte(data), &x.m) != nil {
			continue
		}
		all = append(all, x)
		if x.m.Role == "assistant" {
			answered[x.m.ParentID] = true
			if x.m.Time.Completed == 0 && x.m.Error == nil {
				p := x
				pending = &p
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []protocol.QueuedMessage{}
	if pending == nil {
		return out, nil
	}
	for _, x := range all {
		if x.m.Role != "user" || x.created <= pending.created || answered[x.id] {
			continue
		}
		out = append(out, protocol.QueuedMessage{
			Text:     messageText(db, x.id),
			Delivery: "queue",
			At:       time.UnixMilli(x.created),
		})
	}
	return out, nil
}

// messageText joins a message's text parts.
func messageText(db *sql.DB, messageID string) string {
	rows, err := db.Query(`SELECT data FROM part WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var data string
		var p storedPart
		if rows.Scan(&data) != nil || json.Unmarshal([]byte(data), &p) != nil {
			continue
		}
		if p.Type == "text" && !p.Synthetic && strings.TrimSpace(p.Text) != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(strings.TrimSpace(p.Text))
		}
	}
	return b.String()
}

// promptText is a Prompt payload's text: the JSON object's "text", or the
// value itself if it is not one.
func promptText(payload string) string {
	var p struct {
		Text string `json:"text"`
	}
	if json.Unmarshal([]byte(payload), &p) == nil && p.Text != "" {
		return strings.TrimSpace(p.Text)
	}
	var s string
	if json.Unmarshal([]byte(payload), &s) == nil {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(payload)
}
