package attach

import (
	"fmt"
	"strings"
)

// Input encoding. A phone sends an *intent* ("this text", "escape", "yes"), not
// bytes; the adapter decides the encoding. That indirection is what lets one
// app drive agents whose key handling differs.

// NamedKey is a key a client can send without knowing its byte encoding.
type NamedKey string

const (
	KeyEnter     NamedKey = "enter"
	KeyEscape    NamedKey = "escape"
	KeyTab       NamedKey = "tab"
	KeyBackspace NamedKey = "backspace"
	KeyUp        NamedKey = "up"
	KeyDown      NamedKey = "down"
	KeyLeft      NamedKey = "left"
	KeyRight     NamedKey = "right"
	KeyCtrlC     NamedKey = "ctrl-c"
	KeyCtrlD     NamedKey = "ctrl-d"
	KeyCtrlR     NamedKey = "ctrl-r"
	KeyCtrlL     NamedKey = "ctrl-l"
	// KeyCtrlU clears the composer. On a phone there is no held-down
	// backspace, so without this the only way to abandon a half-typed reply is
	// to send it.
	KeyCtrlU    NamedKey = "ctrl-u"
	KeyPageUp   NamedKey = "pageup"
	KeyPageDown NamedKey = "pagedown"
	KeyHome     NamedKey = "home"
	KeyEnd      NamedKey = "end"
)

var keyBytes = map[NamedKey]string{
	KeyEnter:     "\r",
	KeyEscape:    "\x1b",
	KeyTab:       "\t",
	KeyBackspace: "\x7f",
	KeyUp:        "\x1b[A",
	KeyDown:      "\x1b[B",
	KeyRight:     "\x1b[C",
	KeyLeft:      "\x1b[D",
	KeyCtrlC:     "\x03",
	KeyCtrlD:     "\x04",
	KeyCtrlR:     "\x12",
	KeyCtrlL:     "\x0c",
	KeyCtrlU:     "\x15",
	KeyPageUp:    "\x1b[5~",
	KeyPageDown:  "\x1b[6~",
	KeyHome:      "\x1b[H",
	KeyEnd:       "\x1b[F",
}

// EncodeKey returns the bytes for a named key.
func EncodeKey(k NamedKey) ([]byte, error) {
	b, ok := keyBytes[k]
	if !ok {
		return nil, fmt.Errorf("unknown key %q", k)
	}
	return []byte(b), nil
}

// EncodeKeyMode is EncodeKey for a program that may have asked for
// application cursor keys (DECCKM), as zsh's line editor, vim and less do:
// their arrow keys are ESC O A, not ESC [ A. Sent the wrong way, an arrow in
// vim inserts "A" on a new line.
func EncodeKeyMode(k NamedKey, appCursor bool) ([]byte, error) {
	b, err := EncodeKey(k)
	if err != nil || !appCursor {
		return b, err
	}
	switch k {
	case KeyUp, KeyDown, KeyRight, KeyLeft, KeyHome, KeyEnd:
		return []byte("\x1bO" + string(b[len(b)-1])), nil
	}
	return b, nil
}

// KeyNames lists supported keys, for help text and client capability lists.
func KeyNames() []string {
	out := make([]string, 0, len(keyBytes))
	for k := range keyBytes {
		out = append(out, string(k))
	}
	return out
}

// Bracketed paste markers. Multi-line text must be pasted, not typed: a bare
// newline mid-text submits the prompt in most agent TUIs, so a three-line reply
// would fire as three separate prompts. Bracketed paste tells the TUI "this is
// one block of text" and is why remote multi-line replies work at all.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// EncodeText encodes literal text for an agent's stdin.
//
// submit appends a carriage return to send the prompt. multiline text is
// wrapped in bracketed paste so embedded newlines do not submit early.
func EncodeText(text string, submit bool) []byte {
	var b strings.Builder
	// Normalise CRLF/CR to LF first so the paste block contains exactly what
	// the user typed on a phone keyboard.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	if strings.Contains(text, "\n") {
		b.WriteString(pasteStart)
		b.WriteString(text)
		b.WriteString(pasteEnd)
	} else {
		b.WriteString(text)
	}
	if submit {
		b.WriteString("\r")
	}
	return []byte(b.String())
}

// Fence is a precondition on delivering input.
//
// This is the answer to the offline-queue problem: a reply composed at 09:00
// while in a tunnel must not be injected at 09:20 into a different question.
// If the precondition fails the daemon holds the input and asks the human,
// rather than answering the wrong thing.
type Fence struct {
	// ExpectPromptID must match the session's current prompt id. Empty means
	// unfenced, which is only permitted in expert mode.
	ExpectPromptID string `json:"expect_prompt_id,omitempty"`
	// ExpectState must match the session's current state, if set.
	ExpectState string `json:"expect_state,omitempty"`
}

// FenceError reports why input was withheld.
type FenceError struct {
	Want, Got string
	Field     string
}

func (e *FenceError) Error() string {
	return fmt.Sprintf("fence failed: %s is %q, expected %q", e.Field, e.Got, e.Want)
}

// Check validates a fence against current session facts.
func (f Fence) Check(currentPromptID, currentState string) error {
	if f.ExpectPromptID != "" && f.ExpectPromptID != currentPromptID {
		return &FenceError{Field: "prompt_id", Want: f.ExpectPromptID, Got: currentPromptID}
	}
	if f.ExpectState != "" && f.ExpectState != currentState {
		return &FenceError{Field: "state", Want: f.ExpectState, Got: currentState}
	}
	return nil
}

// IsEmpty reports whether the fence constrains anything.
func (f Fence) IsEmpty() bool { return f.ExpectPromptID == "" && f.ExpectState == "" }
