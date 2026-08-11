package codex

import (
	"strings"
	"testing"
)

// Regression, found by parsing every real rollout on this machine: Codex
// 0.120.0 wrote session_meta.source as an object ({"subagent":"review"}) and
// 0.130.0 writes a string ("cli"). Declaring it as a string made
// json.Unmarshal fail on the whole struct, so one changed field type silently
// discarded the entire header -- session id, cwd and all.
func TestSessionMetaSurvivesSourceShapeChange(t *testing.T) {
	cases := []struct {
		name, line, wantKind, wantSubagent string
	}{
		{
			name:     "0.130.0 string source",
			line:     `{"timestamp":"2026-05-14T16:00:00Z","type":"session_meta","payload":{"id":"s-new","cwd":"/repo","cli_version":"0.130.0","source":"cli"}}`,
			wantKind: "cli",
		},
		{
			name:     "0.120.0 object source",
			line:     `{"timestamp":"2026-04-15T10:59:48Z","type":"session_meta","payload":{"id":"s-old","cwd":"/repo","cli_version":"0.120.0","source":{"subagent":"review"}}}`,
			wantKind: "subagent", wantSubagent: "review",
		},
		{
			name: "absent source",
			line: `{"timestamp":"2026-04-15T10:59:48Z","type":"session_meta","payload":{"id":"s-bare","cwd":"/repo"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, meta, err := ParseRollout(strings.NewReader(c.line + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if meta.SessionID == "" {
				t.Fatal("session id lost; a changed field type must not discard the header")
			}
			if meta.Cwd != "/repo" {
				t.Errorf("cwd = %q", meta.Cwd)
			}
			if meta.Source != c.wantKind {
				t.Errorf("source kind = %q, want %q", meta.Source, c.wantKind)
			}
			if meta.Subagent != c.wantSubagent {
				t.Errorf("subagent = %q, want %q", meta.Subagent, c.wantSubagent)
			}
		})
	}
}

// A subagent session belongs nested under its parent, not listed as a peer.
func TestSubagentIsIdentifiable(t *testing.T) {
	line := `{"type":"session_meta","payload":{"id":"s","cwd":"/r","source":{"subagent":"review"}}}`
	_, meta, err := ParseRollout(strings.NewReader(line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.IsSubagent() {
		t.Error("a subagent session was not identified as one")
	}
	_, plain, _ := ParseRollout(strings.NewReader(
		`{"type":"session_meta","payload":{"id":"s","cwd":"/r","source":"cli"}}` + "\n"))
	if plain.IsSubagent() {
		t.Error("a cli session was misidentified as a subagent")
	}
}
