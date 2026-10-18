package vt

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func feed(e *Emulator, s string) { _, _ = e.Write([]byte(s)) }

func TestPlainText(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "hello world")
	if got := e.Grid().RowText(0); got != "hello world" {
		t.Errorf("row 0 = %q", got)
	}
}

func TestNewlineAndCarriageReturn(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "line one\r\nline two")
	if got := e.Grid().RowText(0); got != "line one" {
		t.Errorf("row 0 = %q", got)
	}
	if got := e.Grid().RowText(1); got != "line two" {
		t.Errorf("row 1 = %q", got)
	}
}

// Carriage return without newline overwrites in place -- how progress bars and
// spinners work, which is most of what an agent TUI emits.
func TestCarriageReturnOverwrite(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "progress 10%\rprogress 99%")
	if got := e.Grid().RowText(0); got != "progress 99%" {
		t.Errorf("row 0 = %q, want the overwritten text", got)
	}
}

func TestCursorPositioning(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "\x1b[3;5Hxy")
	if got := e.Grid().RowText(2); got != "    xy" {
		t.Errorf("row 2 = %q", got)
	}
	if e.Grid().CursorY != 2 || e.Grid().CursorX != 6 {
		t.Errorf("cursor = (%d,%d), want (6,2)", e.Grid().CursorX, e.Grid().CursorY)
	}
}

func TestEraseInLineAndDisplay(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "abcdefgh\x1b[1;4H\x1b[0K") // erase from col 4 to end
	if got := e.Grid().RowText(0); got != "abc" {
		t.Errorf("after EL0 row 0 = %q, want %q", got, "abc")
	}
	feed(e, "\x1b[2J")
	if got := e.Grid().Text(); got != "" {
		t.Errorf("after ED2 screen = %q, want empty", got)
	}
}

func TestSGRColorsAndAttributes(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "\x1b[1;31mred bold\x1b[0m plain")
	c := e.Grid().At(0, 0)
	if !c.Attr.Bold || c.Attr.FG != 1 {
		t.Errorf("first cell attr = %+v, want bold red", c.Attr)
	}
	plain := e.Grid().At(9, 0)
	if plain.Attr != DefaultAttr {
		t.Errorf("after reset attr = %+v, want default", plain.Attr)
	}
}

func TestTruecolorSGR(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "\x1b[38;2;255;128;64mX")
	c := e.Grid().At(0, 0)
	want := int32(255<<16|128<<8|64) | rgbFlag
	if c.Attr.FG != want {
		t.Errorf("fg = %d, want %d", c.Attr.FG, want)
	}
}

func Test256ColorSGR(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "\x1b[38;5;200mX")
	if c := e.Grid().At(0, 0); c.Attr.FG != 200 {
		t.Errorf("fg = %d, want 200", c.Attr.FG)
	}
}

// Unknown escapes must be swallowed, not printed. Garbage on screen is the
// failure mode users notice instantly.
func TestUnknownEscapesAreSwallowed(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "a\x1b[?9999zb\x1b]777;weird\x07c")
	got := e.Grid().RowText(0)
	if strings.ContainsAny(got, "\x1b[]") {
		t.Errorf("escape leaked into the grid: %q", got)
	}
	if got != "abc" {
		t.Errorf("row 0 = %q, want %q", got, "abc")
	}
}

func TestScrollingProducesScrollback(t *testing.T) {
	e := NewEmulator(10, 3)
	for i := 1; i <= 6; i++ {
		feed(e, fmt.Sprintf("line%d\r\n", i))
	}
	// The last rows should hold the most recent lines.
	if got := e.Grid().RowText(0); got != "line5" {
		t.Errorf("row 0 = %q, want line5", got)
	}
	sb := e.Scrollback()
	if len(sb) == 0 {
		t.Fatal("no scrollback captured")
	}
	if sb[0] != "line1" {
		t.Errorf("scrollback[0] = %q, want line1", sb[0])
	}
}

func TestAlternateScreenDoesNotPolluteScrollback(t *testing.T) {
	e := NewEmulator(10, 3)
	feed(e, "real1\r\nreal2\r\n")
	before := len(e.Scrollback())

	feed(e, "\x1b[?1049h") // enter alt screen
	for i := 0; i < 8; i++ {
		feed(e, fmt.Sprintf("tui%d\r\n", i))
	}
	feed(e, "\x1b[?1049l") // leave

	if got := len(e.Scrollback()); got != before {
		t.Errorf("alt-screen output added %d scrollback lines; a full-screen TUI's "+
			"scrolled content is not history", got-before)
	}
	// Main screen content must be intact.
	if !strings.Contains(e.Grid().Text(), "real") {
		t.Errorf("main screen lost after alt-screen round trip: %q", e.Grid().Text())
	}
}

// A character written to the last column must not wrap until the next one
// arrives, or every exactly-full line gains a spurious blank line after it.
func TestDeferredWrap(t *testing.T) {
	e := NewEmulator(5, 4)
	feed(e, "abcde") // exactly fills row 0
	if got := e.Grid().RowText(0); got != "abcde" {
		t.Errorf("row 0 = %q", got)
	}
	if e.Grid().CursorY != 0 {
		t.Errorf("cursor moved to row %d before the next character arrived", e.Grid().CursorY)
	}
	feed(e, "f")
	if got := e.Grid().RowText(1); got != "f" {
		t.Errorf("row 1 = %q, want the wrapped character", got)
	}
}

// CJK and emoji occupy two columns. Getting this wrong shifts every subsequent
// column on the row.
func TestWideRunes(t *testing.T) {
	e := NewEmulator(10, 2)
	feed(e, "日本語")
	if w := RuneWidth('日'); w != 2 {
		t.Fatalf("RuneWidth(日) = %d, want 2", w)
	}
	if !e.Grid().At(1, 0).Wide {
		t.Error("column 1 should be the continuation of a wide rune")
	}
	if got := e.Grid().RowText(0); got != "日本語" {
		t.Errorf("row 0 = %q", got)
	}
	if e.Grid().CursorX != 6 {
		t.Errorf("cursor x = %d, want 6 (3 wide runes)", e.Grid().CursorX)
	}
}

func TestBracketedPasteModeTracked(t *testing.T) {
	e := NewEmulator(10, 2)
	if e.BracketedPaste {
		t.Error("bracketed paste should start disabled")
	}
	feed(e, "\x1b[?2004h")
	if !e.BracketedPaste {
		t.Error("mode 2004 set was not tracked")
	}
	feed(e, "\x1b[?2004l")
	if e.BracketedPaste {
		t.Error("mode 2004 reset was not tracked")
	}
}

func TestOSCTitle(t *testing.T) {
	e := NewEmulator(10, 2)
	feed(e, "\x1b]0;my session\x07")
	if e.Title != "my session" {
		t.Errorf("title = %q", e.Title)
	}
	feed(e, "\x1b]2;second\x1b\\") // ST-terminated
	if e.Title != "second" {
		t.Errorf("ST-terminated title = %q", e.Title)
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	e := NewEmulator(10, 2)
	b := []byte("é")
	// Feed the two bytes of the rune in separate writes, as a PTY read boundary
	// would.
	_, _ = e.Write(b[:1])
	_, _ = e.Write(b[1:])
	if got := e.Grid().RowText(0); got != "é" {
		t.Errorf("row 0 = %q, want é", got)
	}
}

func TestInsertDeleteLines(t *testing.T) {
	e := NewEmulator(10, 4)
	feed(e, "a\r\nb\r\nc\r\n")
	feed(e, "\x1b[1;1H\x1b[1L") // insert a blank line at the top
	if got := e.Grid().RowText(0); got != "" {
		t.Errorf("row 0 = %q, want blank after IL", got)
	}
	if got := e.Grid().RowText(1); got != "a" {
		t.Errorf("row 1 = %q, want a", got)
	}
	feed(e, "\x1b[1;1H\x1b[1M") // delete it again
	if got := e.Grid().RowText(0); got != "a" {
		t.Errorf("row 0 = %q, want a after DL", got)
	}
}

func TestScrollRegion(t *testing.T) {
	e := NewEmulator(10, 5)
	feed(e, "\x1b[2;4r")      // region rows 2-4
	feed(e, "\x1b[1;1Htop")   // outside the region
	feed(e, "\x1b[4;1Hx\r\n") // bottom of region, force a scroll
	feed(e, "y")
	if got := e.Grid().RowText(0); got != "top" {
		t.Errorf("row outside the scroll region was disturbed: %q", got)
	}
}

// ---- the invariant the entire grid path depends on ----

// A snapshot must reconstruct exactly the grid it came from. If this can drift,
// every client eventually renders something the daemon never had.
func TestSnapshotRoundTrip(t *testing.T) {
	e := NewEmulator(40, 10)
	feed(e, "\x1b[1;32mgreen bold\x1b[0m normal\r\n")
	feed(e, "\x1b[38;2;10;20;30mtruecolour\x1b[0m\r\n")
	feed(e, "日本語 wide\r\n")
	feed(e, "\x1b[4munderline\x1b[0m")

	snap := e.Grid().Snapshot()
	rebuilt := ApplySnapshot(snap)

	assertGridsEqual(t, e.Grid(), rebuilt)
}

// Property test: after any random sequence of edits, applying the deltas to a
// client-side replica must reproduce the daemon's grid exactly.
func TestDeltaConvergenceProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const cols, rows = 30, 8

	e := NewEmulator(cols, rows)
	replica := ApplySnapshot(e.Grid().Snapshot())
	prev := e.Grid().Clone()

	words := []string{"alpha", "beta", "gamma", "日本", "x", "hello world", ""}
	escapes := []string{
		"\x1b[0m", "\x1b[1;31m", "\x1b[4m", "\x1b[38;5;99m", "\x1b[7m",
		"\x1b[2J", "\x1b[K", "\x1b[H", "\r\n", "\r", "\x1b[3;7H", "\x1b[2L", "\x1b[1M",
	}

	for round := 0; round < 400; round++ {
		var sb strings.Builder
		for i := 0; i < rng.Intn(5)+1; i++ {
			if rng.Intn(2) == 0 {
				sb.WriteString(escapes[rng.Intn(len(escapes))])
			} else {
				sb.WriteString(words[rng.Intn(len(words))])
			}
		}
		feed(e, sb.String())

		lines := e.Grid().Diff(prev)
		replica.ApplyDelta(Delta{
			Cols: e.Grid().Cols, Rows: e.Grid().Rows,
			CursorX: e.Grid().CursorX, CursorY: e.Grid().CursorY,
			Lines: lines,
		})
		prev = e.Grid().Clone()

		if !gridsEqual(e.Grid(), replica) {
			t.Fatalf("replica diverged at round %d after writing %q\n--- daemon ---\n%s\n--- replica ---\n%s",
				round, sb.String(), e.Grid().Text(), replica.Text())
		}
	}
}

// An unchanged screen must produce no delta rows, or an idle session would
// stream needlessly over cellular.
func TestDiffIsEmptyWhenNothingChanged(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "static content")
	snapshot := e.Grid().Clone()
	if lines := e.Grid().Diff(snapshot); len(lines) != 0 {
		t.Errorf("unchanged grid produced %d delta rows, want 0", len(lines))
	}
}

func TestDiffOnlyChangedRows(t *testing.T) {
	e := NewEmulator(20, 6)
	feed(e, "row0\r\nrow1\r\nrow2")
	prev := e.Grid().Clone()
	feed(e, "\x1b[2;1Hchanged")

	lines := e.Grid().Diff(prev)
	if len(lines) != 1 {
		t.Fatalf("got %d changed rows, want 1: %+v", len(lines), lines)
	}
	if lines[0].Y != 1 {
		t.Errorf("changed row y = %d, want 1", lines[0].Y)
	}
}

func TestLastNonBlankRows(t *testing.T) {
	e := NewEmulator(20, 8)
	feed(e, "one\r\ntwo\r\nthree\r\n")
	got := e.Grid().LastNonBlankRows(2)
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Errorf("LastNonBlankRows(2) = %q", got)
	}
}

func TestResizePreservesContent(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "keep me")
	e.Resize(40, 10)
	if got := e.Grid().RowText(0); got != "keep me" {
		t.Errorf("after resize row 0 = %q", got)
	}
	if e.Grid().Cols != 40 || e.Grid().Rows != 10 {
		t.Errorf("size = %dx%d", e.Grid().Cols, e.Grid().Rows)
	}
}

func gridsEqual(a, b *Grid) bool {
	if a.Cols != b.Cols || a.Rows != b.Rows {
		return false
	}
	for y := 0; y < a.Rows; y++ {
		for x := 0; x < a.Cols; x++ {
			if a.At(x, y) != b.At(x, y) {
				return false
			}
		}
	}
	return true
}

func assertGridsEqual(t *testing.T, want, got *Grid) {
	t.Helper()
	if want.Cols != got.Cols || want.Rows != got.Rows {
		t.Fatalf("size %dx%d != %dx%d", want.Cols, want.Rows, got.Cols, got.Rows)
	}
	for y := 0; y < want.Rows; y++ {
		for x := 0; x < want.Cols; x++ {
			w, g := want.At(x, y), got.At(x, y)
			if w != g {
				t.Fatalf("cell (%d,%d): %+v != %+v\nrow want: %q\nrow got:  %q",
					x, y, w, g, want.RowText(y), got.RowText(y))
			}
		}
	}
}

func BenchmarkEmulatorThroughput(b *testing.B) {
	e := NewEmulator(120, 40)
	chunk := []byte(strings.Repeat("\x1b[32mgreen\x1b[0m some output text here\r\n", 64))
	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = e.Write(chunk)
	}
}

func BenchmarkDiff(b *testing.B) {
	e := NewEmulator(120, 40)
	feed(e, strings.Repeat("some line of terminal output\r\n", 40))
	prev := e.Grid().Clone()
	feed(e, "\x1b[5;1Hchanged line")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.Grid().Diff(prev)
	}
}

// Trailing blank padding is the majority of a typical row on a wide grid and
// costs real bandwidth on cellular, so it must not be transmitted.
func TestSnapshotTrimsTrailingBlanks(t *testing.T) {
	e := NewEmulator(120, 5)
	feed(e, "short line")
	snap := e.Grid().Snapshot()
	if len(snap.Lines) != 1 {
		t.Fatalf("got %d encoded lines, want 1", len(snap.Lines))
	}
	total := 0
	for _, r := range snap.Lines[0].Runs {
		total += len(r.Text)
	}
	if total > len("short line") {
		t.Errorf("encoded %d chars for a %d-char line; trailing blanks were sent",
			total, len("short line"))
	}
	// And it must still round-trip exactly.
	assertGridsEqual(t, e.Grid(), ApplySnapshot(snap))
}

// A trailing run with a background colour is visible and must survive.
func TestSnapshotKeepsColouredTrailingSpaces(t *testing.T) {
	e := NewEmulator(20, 3)
	feed(e, "x\x1b[41m    ") // red background spaces
	snap := e.Grid().Snapshot()
	assertGridsEqual(t, e.Grid(), ApplySnapshot(snap))
	var sawColoured bool
	for _, r := range snap.Lines[0].Runs {
		if r.Attr.BG == 1 && len(r.Text) > 0 {
			sawColoured = true
		}
	}
	if !sawColoured {
		t.Error("coloured trailing spaces were trimmed; they are visible on screen")
	}
}

// A program that asks where the cursor is waits for the answer. fzf does this
// on start, and without a reply it sat on a blank screen.
func TestQueriesAreAnswered(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "ab\r\nc\x1b[6n")
	if got := string(e.TakeReplies()); got != "\x1b[2;2R" {
		t.Errorf("cursor report = %q, want ESC[2;2R", got)
	}
	feed(e, "\x1b[5n\x1b[c\x1b[>c")
	if got := string(e.TakeReplies()); got != "\x1b[0n\x1b[?62;22c\x1b[>0;10;1c" {
		t.Errorf("replies = %q", got)
	}
	if got := e.TakeReplies(); len(got) != 0 {
		t.Errorf("replies were not forgotten once taken: %q", got)
	}
}

func TestApplicationCursorKeysAreTracked(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "\x1b[?1h")
	if !e.AppCursor {
		t.Fatal("ESC[?1h did not set application cursor keys")
	}
	feed(e, "\x1b[?1l")
	if e.AppCursor {
		t.Fatal("ESC[?1l did not clear application cursor keys")
	}
}

// vim sends ESC[>4;2m on start. Read as SGR, everything after it was dim.
func TestModifyOtherKeysIsNotARendition(t *testing.T) {
	e := NewEmulator(20, 5)
	feed(e, "\x1b[1m\x1b[>4;2mx")
	a := e.Grid().At(0, 0).Attr
	if a.Dim || !a.Bold {
		t.Errorf("attr after ESC[>4;2m = %+v, want bold and not dim", a)
	}
}

// The keyboard opening on a phone shrinks the terminal. The line being typed
// on has to survive it.
func TestShrinkingKeepsTheCursorsLine(t *testing.T) {
	e := NewEmulator(20, 6)
	feed(e, "one\r\ntwo\r\nthree\r\nfour\r\n$ typing")
	e.Resize(20, 3)
	if got := e.Grid().RowText(2); got != "$ typing" {
		t.Errorf("bottom row after shrinking = %q, want the prompt", got)
	}
	if g := e.Grid(); g.CursorY != 2 || g.CursorX != 8 {
		t.Errorf("cursor = %d,%d, want 8,2", g.CursorX, g.CursorY)
	}
	if sb := e.Scrollback(); len(sb) != 2 || sb[0] != "one" {
		t.Errorf("scrollback = %q, want the lines pushed off the top", sb)
	}
}
