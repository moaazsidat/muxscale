// Package vt implements a headless VT100/xterm emulator and grid diffing.
//
// The daemon maintains screen state so clients receive a *grid* rather than a
// byte stream. That matters most on a phone: applying row deltas to a model is
// far cheaper than replaying ANSI into a JS terminal emulator, and it is
// self-healing -- a lost delta is repaired by requesting a fresh snapshot,
// rather than corrupting the display forever.
package vt

import (
	"strings"
	"unicode/utf8"
)

// Attr is a cell's rendition.
type Attr struct {
	FG, BG    int32 // -1 default; 0-255 palette; else 0xRRGGBB | rgbFlag
	Bold      bool
	Italic    bool
	Underline bool
	Reverse   bool
	Dim       bool
	Strike    bool
}

// rgbFlag marks a colour as 24-bit rather than a palette index.
const rgbFlag = 1 << 24

// DefaultAttr is the reset rendition.
var DefaultAttr = Attr{FG: -1, BG: -1}

// Cell is one screen position.
type Cell struct {
	Rune rune
	Attr Attr
	// Wide marks the second column of a double-width character, which holds no
	// rune of its own. Without this, CJK text drifts by one column per glyph.
	Wide bool
}

// blank is an empty cell.
var blank = Cell{Rune: ' ', Attr: DefaultAttr}

// Grid is a screen buffer.
type Grid struct {
	Cols, Rows int
	cells      []Cell // row-major, len == Cols*Rows
	CursorX    int
	CursorY    int
	CursorOff  bool
}

// NewGrid builds a blank grid.
func NewGrid(cols, rows int) *Grid {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	g := &Grid{Cols: cols, Rows: rows, cells: make([]Cell, cols*rows)}
	g.Clear()
	return g
}

// Clear resets every cell.
func (g *Grid) Clear() {
	for i := range g.cells {
		g.cells[i] = blank
	}
	g.CursorX, g.CursorY = 0, 0
}

// At returns the cell at (x, y).
func (g *Grid) At(x, y int) Cell {
	if x < 0 || y < 0 || x >= g.Cols || y >= g.Rows {
		return blank
	}
	return g.cells[y*g.Cols+x]
}

// Set writes a cell.
func (g *Grid) Set(x, y int, c Cell) {
	if x < 0 || y < 0 || x >= g.Cols || y >= g.Rows {
		return
	}
	g.cells[y*g.Cols+x] = c
}

// Row returns a copy of one row.
func (g *Grid) Row(y int) []Cell {
	if y < 0 || y >= g.Rows {
		return nil
	}
	out := make([]Cell, g.Cols)
	copy(out, g.cells[y*g.Cols:(y+1)*g.Cols])
	return out
}

// RowText renders a row as plain text with trailing blanks trimmed.
// Prompt matching and state inference run against this.
func (g *Grid) RowText(y int) string {
	if y < 0 || y >= g.Rows {
		return ""
	}
	var b strings.Builder
	for x := 0; x < g.Cols; x++ {
		c := g.cells[y*g.Cols+x]
		if c.Wide {
			continue // the wide rune was emitted by its first column
		}
		b.WriteRune(c.Rune)
	}
	return strings.TrimRight(b.String(), " ")
}

// Text renders the whole grid, trailing blank lines trimmed.
func (g *Grid) Text() string {
	lines := make([]string, g.Rows)
	for y := 0; y < g.Rows; y++ {
		lines[y] = g.RowText(y)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// LastNonBlankRows returns up to n trailing non-blank rows, which is the window
// prompt matchers examine.
func (g *Grid) LastNonBlankRows(n int) []string {
	var out []string
	for y := g.Rows - 1; y >= 0 && len(out) < n; y-- {
		t := g.RowText(y)
		if t == "" && len(out) == 0 {
			continue // skip trailing blanks
		}
		out = append([]string{t}, out...)
	}
	return out
}

// Resize changes dimensions, preserving overlapping content.
func (g *Grid) Resize(cols, rows int) {
	if cols <= 0 || rows <= 0 || (cols == g.Cols && rows == g.Rows) {
		return
	}
	next := make([]Cell, cols*rows)
	for i := range next {
		next[i] = blank
	}
	for y := 0; y < min(rows, g.Rows); y++ {
		for x := 0; x < min(cols, g.Cols); x++ {
			next[y*cols+x] = g.cells[y*g.Cols+x]
		}
	}
	g.cells, g.Cols, g.Rows = next, cols, rows
	g.CursorX = min(g.CursorX, cols-1)
	g.CursorY = min(g.CursorY, rows-1)
}

// Clone returns a deep copy, used as the diff baseline.
func (g *Grid) Clone() *Grid {
	c := &Grid{
		Cols: g.Cols, Rows: g.Rows,
		cells:   make([]Cell, len(g.cells)),
		CursorX: g.CursorX, CursorY: g.CursorY, CursorOff: g.CursorOff,
	}
	copy(c.cells, g.cells)
	return c
}

// ---- wire representation ----

// Run is a horizontal span of cells sharing one rendition, which is how a
// screenful of mostly-uniform text compresses well enough for cellular.
type Run struct {
	Text string `json:"t"`
	Attr Attr   `json:"a"`
}

// RowRuns is one row encoded as runs.
type RowRuns struct {
	Y    int   `json:"y"`
	Runs []Run `json:"r"`
}

// Snapshot is a full screen state.
type Snapshot struct {
	Cols      int       `json:"cols"`
	Rows      int       `json:"rows"`
	CursorX   int       `json:"cx"`
	CursorY   int       `json:"cy"`
	CursorOff bool      `json:"coff,omitempty"`
	Lines     []RowRuns `json:"lines"`
}

// Delta is the set of rows that changed since a baseline.
type Delta struct {
	BaseSeq   uint64    `json:"base_seq"`
	Seq       uint64    `json:"seq"`
	Cols      int       `json:"cols"`
	Rows      int       `json:"rows"`
	CursorX   int       `json:"cx"`
	CursorY   int       `json:"cy"`
	CursorOff bool      `json:"coff,omitempty"`
	Lines     []RowRuns `json:"lines"`
}

// encodeRow run-length encodes a row by rendition.
func (g *Grid) encodeRow(y int) []Run {
	var runs []Run
	var cur strings.Builder
	var curAttr Attr
	started := false

	flush := func() {
		if started && cur.Len() > 0 {
			runs = append(runs, Run{Text: cur.String(), Attr: curAttr})
		}
		cur.Reset()
	}

	for x := 0; x < g.Cols; x++ {
		c := g.cells[y*g.Cols+x]
		if c.Wide {
			continue
		}
		if !started || c.Attr != curAttr {
			flush()
			curAttr = c.Attr
			started = true
		}
		cur.WriteRune(c.Rune)
	}
	flush()

	// Trim trailing default-rendition spaces. On a 120-column grid these are
	// the majority of a typical row, and the client pads with blanks anyway, so
	// sending them is pure cellular cost. Only default-rendition spaces are
	// safe to drop: a trailing run with a background colour is visible.
	if n := len(runs); n > 0 && runs[n-1].Attr == DefaultAttr {
		trimmed := strings.TrimRight(runs[n-1].Text, " ")
		if trimmed == "" {
			runs = runs[:n-1]
		} else {
			runs[n-1].Text = trimmed
		}
	}
	return runs
}

// Snapshot encodes the full grid.
func (g *Grid) Snapshot() Snapshot {
	s := Snapshot{
		Cols: g.Cols, Rows: g.Rows,
		CursorX: g.CursorX, CursorY: g.CursorY, CursorOff: g.CursorOff,
	}
	for y := 0; y < g.Rows; y++ {
		if runs := g.encodeRow(y); len(runs) > 0 {
			s.Lines = append(s.Lines, RowRuns{Y: y, Runs: runs})
		}
	}
	return s
}

// Diff returns the rows that differ from prev.
//
// A dimension change returns every row: the client must rebuild anyway, and a
// partial update against a differently-sized grid is how corruption starts.
func (g *Grid) Diff(prev *Grid) []RowRuns {
	var out []RowRuns
	if prev == nil || prev.Cols != g.Cols || prev.Rows != g.Rows {
		for y := 0; y < g.Rows; y++ {
			out = append(out, RowRuns{Y: y, Runs: g.encodeRow(y)})
		}
		return out
	}
	for y := 0; y < g.Rows; y++ {
		if !rowEqual(g.cells[y*g.Cols:(y+1)*g.Cols], prev.cells[y*prev.Cols:(y+1)*prev.Cols]) {
			out = append(out, RowRuns{Y: y, Runs: g.encodeRow(y)})
		}
	}
	return out
}

func rowEqual(a, b []Cell) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ApplySnapshot rebuilds a grid from a snapshot. Clients use this; the daemon
// uses it in tests to assert that what it sends reconstructs what it has --
// the invariant the whole grid path depends on.
func ApplySnapshot(s Snapshot) *Grid {
	g := NewGrid(s.Cols, s.Rows)
	g.CursorX, g.CursorY, g.CursorOff = s.CursorX, s.CursorY, s.CursorOff
	for _, line := range s.Lines {
		applyRuns(g, line)
	}
	return g
}

// ApplyDelta applies changed rows to a grid.
func (g *Grid) ApplyDelta(d Delta) {
	if d.Cols != g.Cols || d.Rows != g.Rows {
		g.Resize(d.Cols, d.Rows)
	}
	for _, line := range d.Lines {
		applyRuns(g, line)
	}
	g.CursorX, g.CursorY, g.CursorOff = d.CursorX, d.CursorY, d.CursorOff
}

func applyRuns(g *Grid, line RowRuns) {
	if line.Y < 0 || line.Y >= g.Rows {
		return
	}
	x := 0
	for _, run := range line.Runs {
		for _, r := range run.Text {
			if x >= g.Cols {
				break
			}
			g.Set(x, line.Y, Cell{Rune: r, Attr: run.Attr})
			x++
			if RuneWidth(r) == 2 && x < g.Cols {
				g.Set(x, line.Y, Cell{Rune: 0, Attr: run.Attr, Wide: true})
				x++
			}
		}
	}
	for ; x < g.Cols; x++ {
		g.Set(x, line.Y, blank)
	}
}

// RuneWidth reports display columns: 0 for combining marks, 2 for wide CJK and
// emoji, else 1. Getting this wrong shifts every subsequent column on the row.
func RuneWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if !utf8.ValidRune(r) {
		return 1
	}
	switch {
	case r < 0x20 || (r >= 0x7f && r < 0xa0):
		return 0
	case isCombining(r):
		return 0
	case isWide(r):
		return 2
	default:
		return 1
	}
}

func isCombining(r rune) bool {
	return (r >= 0x0300 && r <= 0x036f) ||
		(r >= 0x1ab0 && r <= 0x1aff) ||
		(r >= 0x20d0 && r <= 0x20ff) ||
		(r >= 0xfe00 && r <= 0xfe0f) || // variation selectors
		(r >= 0xfe20 && r <= 0xfe2f)
}

func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0x303e, // CJK radicals, Kangxi
		r >= 0x3041 && r <= 0x33ff, // Hiragana .. CJK compat
		r >= 0x3400 && r <= 0x4dbf, // CJK ext A
		r >= 0x4e00 && r <= 0x9fff, // CJK unified
		r >= 0xa000 && r <= 0xa4cf, // Yi
		r >= 0xac00 && r <= 0xd7a3, // Hangul syllables
		r >= 0xf900 && r <= 0xfaff, // CJK compat ideographs
		r >= 0xfe30 && r <= 0xfe6f, // CJK compat forms
		r >= 0xff00 && r <= 0xff60, // fullwidth forms
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, // emoji
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd:
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
