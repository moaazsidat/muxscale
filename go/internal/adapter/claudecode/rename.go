package claudecode

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// SetCustomTitle names a session the way Claude Code's own /rename does, so
// the name shows in `claude --resume` and survives into the session's next
// run, not only in muxscale.
//
// /rename does two things, and this mirrors both: it appends a custom-title
// entry to the transcript, which is the record (last one wins), and it writes
// a custom-title.json sidecar in the session's directory beside it, which the
// resume picker reads without opening the transcript. An empty title clears
// the name: the entry is still appended, so it overrides an earlier one, and
// the sidecar is removed.
//
// A running Claude Code process does not see this until it next reads the
// file. It holds its own copy of the title and may write it out again later,
// and since the last entry wins, that would undo the rename. It is the same
// race as two terminals renaming one session, and settles the same way.
func SetCustomTitle(transcriptPath, sessionID, title string) error {
	line, err := json.Marshal(struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}{"custom-title", title, sessionID})
	if err != nil {
		return err
	}
	// Append-only and one write, as Claude Code itself writes: the line lands
	// whole between its own, never inside one.
	f, err := os.OpenFile(transcriptPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	sidecar := filepath.Join(filepath.Dir(transcriptPath), sessionID, "custom-title.json")
	if title == "" {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	body, err := json.Marshal(struct {
		CustomTitle string `json:"customTitle"`
	}{title})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o700); err != nil {
		return err
	}
	return os.WriteFile(sidecar, body, 0o600)
}
