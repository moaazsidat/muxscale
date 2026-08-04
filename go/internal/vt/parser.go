package vt

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// The VT parser. A state machine over the byte stream, deliberately covering
// the subset agent TUIs actually emit rather than every escape ever defined:
// SGR, cursor motion, erase, scroll regions, alternate screen, and bracketed
// paste. Unknown sequences are consumed and ignored -- never printed as
// garbage, which is the failure mode users notice immediately.

type parseState int

const (
	stateGround parseState = iota
	stateEscape
	stateCSI
	stateOSC
	stateDCS
)

// Emulator turns a byte stream into grid state.
type Emulator struct {
	grid *Grid
	alt  *Grid // alternate screen buffer
	main *Grid

	state  parseState
	params []byte
	interm []byte
	osc    []byte
	utf8   []byte

	attr        Attr
	savedX      int
	savedY      int
	savedAttr   Attr
	scrollTop   int
	scrollBot   int
	inAltScreen bool
	wrapPending bool

	// Scrollback holds lines that scrolled off the top.
	scrollback []string
	maxScroll  int

	// Title is the last OSC 0/2 window title seen. Agents often set this to
	// something useful, which is a free session label.
	Title string
	// LastPromptRow is the row of the most recent OSC 133 prompt mark. When a
	// shell emits these, prompt detection is exact rather than heuristic.
	LastPromptRow int

	// BracketedPaste reports whether the app enabled paste bracketing. Input
	// encoders check this: sending paste markers to an app that has not asked
	// for them makes the markers appear as literal text.
	BracketedPaste bool
}

// NewEmulator builds an emulator with a blank screen.
func NewEmulator(cols, rows int) *Emulator {
	g := NewGrid(cols, rows)
	e := &Emulator{
		grid: g, main: g, attr: DefaultAttr,
		scrollTop: 0, scrollBot: rows - 1, maxScroll: 5000,
	}
	return e
}

// Grid returns the active screen.
func (e *Emulator) Grid() *Grid { return e.grid }

// Scrollback returns lines that scrolled off, oldest first.
func (e *Emulator) Scrollback() []string { return e.scrollback }

// Resize changes the screen size.
func (e *Emulator) Resize(cols, rows int) {
	e.main.Resize(cols, rows)
	if e.alt != nil {
		e.alt.Resize(cols, rows)
	}
	e.scrollTop, e.scrollBot = 0, rows-1
}

// Write feeds bytes into the emulator.
func (e *Emulator) Write(p []byte) (int, error) {
	for _, b := range p {
		e.step(b)
	}
	return len(p), nil
}

func (e *Emulator) step(b byte) {
	switch e.state {
	case stateGround:
		e.ground(b)
	case stateEscape:
		e.escape(b)
	case stateCSI:
		e.csi(b)
	case stateOSC:
		e.oscByte(b)
	case stateDCS:
		// Consume until ST; DCS payloads are not rendered.
		if b == 0x9c || (len(e.osc) > 0 && e.osc[len(e.osc)-1] == 0x1b && b == '\\') {
			e.state = stateGround
			e.osc = e.osc[:0]
			return
		}
		e.osc = append(e.osc, b)
	}
}

func (e *Emulator) ground(b byte) {
	switch {
	case b == 0x1b:
		e.state = stateEscape
		e.params = e.params[:0]
		e.interm = e.interm[:0]
	case b == '\n':
		e.lineFeed()
	case b == '\r':
		e.grid.CursorX = 0
		e.wrapPending = false
	case b == '\t':
		next := ((e.grid.CursorX / 8) + 1) * 8
		e.grid.CursorX = min(next, e.grid.Cols-1)
	case b == '\b':
		if e.grid.CursorX > 0 {
			e.grid.CursorX--
		}
		e.wrapPending = false
	case b == 0x07: // bell
	case b < 0x20:
		// Other control characters are not rendered.
	default:
		e.printByte(b)
	}
}

// printByte accumulates UTF-8 and writes complete runes.
func (e *Emulator) printByte(b byte) {
	e.utf8 = append(e.utf8, b)
	if !utf8.FullRune(e.utf8) {
		if len(e.utf8) > 4 { // invalid; drop rather than grow forever
			e.utf8 = e.utf8[:0]
		}
		return
	}
	r, _ := utf8.DecodeRune(e.utf8)
	e.utf8 = e.utf8[:0]
	if r == utf8.RuneError {
		return
	}
	e.printRune(r)
}

func (e *Emulator) printRune(r rune) {
	w := RuneWidth(r)
	if w == 0 {
		return // combining mark: ignored rather than misplaced
	}

	// Deferred wrap: a character written to the last column does not wrap until
	// the *next* character arrives. Wrapping eagerly would insert a spurious
	// blank line every time a line exactly fills the width.
	if e.wrapPending {
		e.grid.CursorX = 0
		e.lineFeed()
		e.wrapPending = false
	}
	if e.grid.CursorX+w > e.grid.Cols {
		e.grid.CursorX = 0
		e.lineFeed()
	}

	// Overwriting either half of a double-width character must clear the other
	// half. A real terminal does this; skipping it leaves an orphaned
	// continuation cell that renders as a phantom column and, worse, cannot be
	// reproduced from the wire encoding -- which is how a client's replica
	// silently drifts out of sync with the daemon.
	e.clearWidePartner(e.grid.CursorX, e.grid.CursorY)
	if w == 2 {
		e.clearWidePartner(e.grid.CursorX+1, e.grid.CursorY)
	}

	e.grid.Set(e.grid.CursorX, e.grid.CursorY, Cell{Rune: r, Attr: e.attr})
	e.grid.CursorX++
	if w == 2 {
		e.grid.Set(e.grid.CursorX, e.grid.CursorY, Cell{Rune: 0, Attr: e.attr, Wide: true})
		e.grid.CursorX++
	}
	if e.grid.CursorX >= e.grid.Cols {
		e.grid.CursorX = e.grid.Cols - 1
		e.wrapPending = true
	}
}

// clearWidePartner blanks the other half of a double-width pair that overlaps
// position (x, y), so no orphaned continuation cell is left behind.
func (e *Emulator) clearWidePartner(x, y int) {
	if x < 0 || x >= e.grid.Cols {
		return
	}
	c := e.grid.At(x, y)
	if c.Wide {
		// We are landing on a continuation cell; clear its leader to the left.
		if x > 0 {
			e.grid.Set(x-1, y, Cell{Rune: ' ', Attr: e.attr})
		}
		e.grid.Set(x, y, Cell{Rune: ' ', Attr: e.attr})
		return
	}
	if RuneWidth(c.Rune) == 2 && x+1 < e.grid.Cols {
		// We are landing on a wide leader; clear its continuation to the right.
		e.grid.Set(x+1, y, Cell{Rune: ' ', Attr: e.attr})
	}
}

func (e *Emulator) lineFeed() {
	e.wrapPending = false
	if e.grid.CursorY == e.scrollBot {
		e.scrollUp(1)
		return
	}
	if e.grid.CursorY < e.grid.Rows-1 {
		e.grid.CursorY++
	}
}

func (e *Emulator) scrollUp(n int) {
	for i := 0; i < n; i++ {
		// Only the main screen contributes scrollback; the alternate screen is
		// for full-screen apps whose scrolled content is not history.
		if !e.inAltScreen {
			if line := e.grid.RowText(e.scrollTop); true {
				e.scrollback = append(e.scrollback, line)
				if len(e.scrollback) > e.maxScroll {
					e.scrollback = e.scrollback[len(e.scrollback)-e.maxScroll:]
				}
			}
		}
		for y := e.scrollTop; y < e.scrollBot; y++ {
			copy(e.grid.cells[y*e.grid.Cols:(y+1)*e.grid.Cols],
				e.grid.cells[(y+1)*e.grid.Cols:(y+2)*e.grid.Cols])
		}
		e.clearRow(e.scrollBot)
	}
}

func (e *Emulator) scrollDown(n int) {
	for i := 0; i < n; i++ {
		for y := e.scrollBot; y > e.scrollTop; y-- {
			copy(e.grid.cells[y*e.grid.Cols:(y+1)*e.grid.Cols],
				e.grid.cells[(y-1)*e.grid.Cols:y*e.grid.Cols])
		}
		e.clearRow(e.scrollTop)
	}
}

func (e *Emulator) clearRow(y int) {
	if y < 0 || y >= e.grid.Rows {
		return
	}
	for x := 0; x < e.grid.Cols; x++ {
		e.grid.cells[y*e.grid.Cols+x] = Cell{Rune: ' ', Attr: e.attr}
	}
}

func (e *Emulator) escape(b byte) {
	switch b {
	case '[':
		e.state = stateCSI
		e.params = e.params[:0]
		e.interm = e.interm[:0]
	case ']':
		e.state = stateOSC
		e.osc = e.osc[:0]
	case 'P':
		e.state = stateDCS
		e.osc = e.osc[:0]
	case '7':
		e.savedX, e.savedY, e.savedAttr = e.grid.CursorX, e.grid.CursorY, e.attr
		e.state = stateGround
	case '8':
		e.grid.CursorX, e.grid.CursorY, e.attr = e.savedX, e.savedY, e.savedAttr
		e.state = stateGround
	case 'M': // reverse index
		if e.grid.CursorY == e.scrollTop {
			e.scrollDown(1)
		} else if e.grid.CursorY > 0 {
			e.grid.CursorY--
		}
		e.state = stateGround
	case 'D':
		e.lineFeed()
		e.state = stateGround
	case 'c': // full reset
		e.grid.Clear()
		e.attr = DefaultAttr
		e.state = stateGround
	default:
		e.state = stateGround
	}
}

func (e *Emulator) oscByte(b byte) {
	// OSC ends at BEL or ST (ESC \).
	if b == 0x07 {
		e.handleOSC(string(e.osc))
		e.osc = e.osc[:0]
		e.state = stateGround
		return
	}
	if b == '\\' && len(e.osc) > 0 && e.osc[len(e.osc)-1] == 0x1b {
		e.handleOSC(string(e.osc[:len(e.osc)-1]))
		e.osc = e.osc[:0]
		e.state = stateGround
		return
	}
	if len(e.osc) < 4096 {
		e.osc = append(e.osc, b)
	}
}

func (e *Emulator) handleOSC(s string) {
	parts := strings.SplitN(s, ";", 2)
	if len(parts) != 2 {
		return
	}
	switch parts[0] {
	case "0", "2":
		e.Title = parts[1]
	case "133":
		// Semantic prompt marks (OSC 133). When a shell emits these, prompt
		// detection becomes exact rather than heuristic.
		switch {
		case strings.HasPrefix(parts[1], "A"):
			e.LastPromptRow = e.grid.CursorY
		}
	}
}

func (e *Emulator) csi(b byte) {
	switch {
	case b >= 0x30 && b <= 0x3f: // parameter bytes
		e.params = append(e.params, b)
		return
	case b >= 0x20 && b <= 0x2f: // intermediate bytes
		e.interm = append(e.interm, b)
		return
	}
	e.dispatchCSI(b)
	e.state = stateGround
}

func (e *Emulator) params1(def int) int {
	ps := e.paramList()
	if len(ps) == 0 || ps[0] == 0 {
		if len(ps) > 0 && ps[0] == 0 && def == 0 {
			return 0
		}
		return def
	}
	return ps[0]
}

func (e *Emulator) paramList() []int {
	s := string(e.params)
	private := strings.HasPrefix(s, "?")
	s = strings.TrimPrefix(s, "?")
	_ = private
	if s == "" {
		return nil
	}
	fields := strings.Split(s, ";")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, _ := strconv.Atoi(f)
		out = append(out, n)
	}
	return out
}

func (e *Emulator) isPrivate() bool { return strings.HasPrefix(string(e.params), "?") }

func (e *Emulator) dispatchCSI(cmd byte) {
	g := e.grid
	ps := e.paramList()

	switch cmd {
	case 'A': // cursor up
		g.CursorY = max(0, g.CursorY-e.params1(1))
	case 'B': // cursor down
		g.CursorY = min(g.Rows-1, g.CursorY+e.params1(1))
	case 'C': // cursor forward
		g.CursorX = min(g.Cols-1, g.CursorX+e.params1(1))
		e.wrapPending = false
	case 'D': // cursor back
		g.CursorX = max(0, g.CursorX-e.params1(1))
		e.wrapPending = false
	case 'E':
		g.CursorY = min(g.Rows-1, g.CursorY+e.params1(1))
		g.CursorX = 0
	case 'F':
		g.CursorY = max(0, g.CursorY-e.params1(1))
		g.CursorX = 0
	case 'G', '`': // absolute column
		g.CursorX = clamp(e.params1(1)-1, 0, g.Cols-1)
	case 'd': // absolute row
		g.CursorY = clamp(e.params1(1)-1, 0, g.Rows-1)
	case 'H', 'f': // cursor position
		row, col := 1, 1
		if len(ps) > 0 {
			row = ps[0]
		}
		if len(ps) > 1 {
			col = ps[1]
		}
		g.CursorY = clamp(row-1, 0, g.Rows-1)
		g.CursorX = clamp(col-1, 0, g.Cols-1)
		e.wrapPending = false
	case 'J': // erase in display
		e.eraseDisplay(e.params1(0))
	case 'K': // erase in line
		e.eraseLine(e.params1(0))
	case 'L': // insert lines
		e.insertLines(e.params1(1))
	case 'M': // delete lines
		e.deleteLines(e.params1(1))
	case 'P': // delete chars
		e.deleteChars(e.params1(1))
	case '@': // insert blanks
		e.insertChars(e.params1(1))
	case 'X': // erase chars
		n := e.params1(1)
		for i := 0; i < n && g.CursorX+i < g.Cols; i++ {
			g.Set(g.CursorX+i, g.CursorY, Cell{Rune: ' ', Attr: e.attr})
		}
	case 'S':
		e.scrollUp(e.params1(1))
	case 'T':
		e.scrollDown(e.params1(1))
	case 'r': // set scroll region
		top, bot := 1, g.Rows
		if len(ps) > 0 {
			top = ps[0]
		}
		if len(ps) > 1 {
			bot = ps[1]
		}
		e.scrollTop = clamp(top-1, 0, g.Rows-1)
		e.scrollBot = clamp(bot-1, 0, g.Rows-1)
		if e.scrollTop >= e.scrollBot {
			e.scrollTop, e.scrollBot = 0, g.Rows-1
		}
		g.CursorX, g.CursorY = 0, e.scrollTop
	case 'm': // SGR
		e.applySGR(ps)
	case 'h':
		e.setMode(ps, true)
	case 'l':
		e.setMode(ps, false)
	case 's':
		e.savedX, e.savedY = g.CursorX, g.CursorY
	case 'u':
		g.CursorX, g.CursorY = e.savedX, e.savedY
	}
}

func (e *Emulator) setMode(ps []int, on bool) {
	if !e.isPrivate() {
		return
	}
	for _, p := range ps {
		switch p {
		case 25: // cursor visibility
			e.grid.CursorOff = !on
		case 1049, 1047, 47: // alternate screen
			e.setAltScreen(on)
		case 2004: // bracketed paste
			e.BracketedPaste = on
		}
	}
}

func (e *Emulator) setAltScreen(on bool) {
	if on == e.inAltScreen {
		return
	}
	if on {
		if e.alt == nil {
			e.alt = NewGrid(e.main.Cols, e.main.Rows)
		}
		e.alt.Clear()
		e.grid = e.alt
		e.inAltScreen = true
	} else {
		e.grid = e.main
		e.inAltScreen = false
	}
	e.scrollTop, e.scrollBot = 0, e.grid.Rows-1
}

func (e *Emulator) eraseDisplay(mode int) {
	g := e.grid
	switch mode {
	case 0: // cursor to end
		for x := g.CursorX; x < g.Cols; x++ {
			g.Set(x, g.CursorY, Cell{Rune: ' ', Attr: e.attr})
		}
		for y := g.CursorY + 1; y < g.Rows; y++ {
			e.clearRow(y)
		}
	case 1: // start to cursor
		for y := 0; y < g.CursorY; y++ {
			e.clearRow(y)
		}
		for x := 0; x <= g.CursorX && x < g.Cols; x++ {
			g.Set(x, g.CursorY, Cell{Rune: ' ', Attr: e.attr})
		}
	case 2, 3: // whole screen
		for y := 0; y < g.Rows; y++ {
			e.clearRow(y)
		}
	}
}

func (e *Emulator) eraseLine(mode int) {
	g := e.grid
	switch mode {
	case 0:
		for x := g.CursorX; x < g.Cols; x++ {
			g.Set(x, g.CursorY, Cell{Rune: ' ', Attr: e.attr})
		}
	case 1:
		for x := 0; x <= g.CursorX && x < g.Cols; x++ {
			g.Set(x, g.CursorY, Cell{Rune: ' ', Attr: e.attr})
		}
	case 2:
		e.clearRow(g.CursorY)
	}
}

func (e *Emulator) insertLines(n int) {
	g := e.grid
	if g.CursorY < e.scrollTop || g.CursorY > e.scrollBot {
		return
	}
	for i := 0; i < n; i++ {
		for y := e.scrollBot; y > g.CursorY; y-- {
			copy(g.cells[y*g.Cols:(y+1)*g.Cols], g.cells[(y-1)*g.Cols:y*g.Cols])
		}
		e.clearRow(g.CursorY)
	}
}

func (e *Emulator) deleteLines(n int) {
	g := e.grid
	if g.CursorY < e.scrollTop || g.CursorY > e.scrollBot {
		return
	}
	for i := 0; i < n; i++ {
		for y := g.CursorY; y < e.scrollBot; y++ {
			copy(g.cells[y*g.Cols:(y+1)*g.Cols], g.cells[(y+1)*g.Cols:(y+2)*g.Cols])
		}
		e.clearRow(e.scrollBot)
	}
}

func (e *Emulator) deleteChars(n int) {
	g := e.grid
	row := g.cells[g.CursorY*g.Cols : (g.CursorY+1)*g.Cols]
	for i := 0; i < n; i++ {
		copy(row[g.CursorX:], row[g.CursorX+1:])
		row[g.Cols-1] = Cell{Rune: ' ', Attr: e.attr}
	}
}

func (e *Emulator) insertChars(n int) {
	g := e.grid
	row := g.cells[g.CursorY*g.Cols : (g.CursorY+1)*g.Cols]
	for i := 0; i < n; i++ {
		copy(row[g.CursorX+1:], row[g.CursorX:])
		row[g.CursorX] = Cell{Rune: ' ', Attr: e.attr}
	}
}

func (e *Emulator) applySGR(ps []int) {
	if len(ps) == 0 {
		e.attr = DefaultAttr
		return
	}
	for i := 0; i < len(ps); i++ {
		p := ps[i]
		switch {
		case p == 0:
			e.attr = DefaultAttr
		case p == 1:
			e.attr.Bold = true
		case p == 2:
			e.attr.Dim = true
		case p == 3:
			e.attr.Italic = true
		case p == 4:
			e.attr.Underline = true
		case p == 7:
			e.attr.Reverse = true
		case p == 9:
			e.attr.Strike = true
		case p == 22:
			e.attr.Bold, e.attr.Dim = false, false
		case p == 23:
			e.attr.Italic = false
		case p == 24:
			e.attr.Underline = false
		case p == 27:
			e.attr.Reverse = false
		case p == 29:
			e.attr.Strike = false
		case p >= 30 && p <= 37:
			e.attr.FG = int32(p - 30)
		case p == 39:
			e.attr.FG = -1
		case p >= 40 && p <= 47:
			e.attr.BG = int32(p - 40)
		case p == 49:
			e.attr.BG = -1
		case p >= 90 && p <= 97:
			e.attr.FG = int32(p - 90 + 8)
		case p >= 100 && p <= 107:
			e.attr.BG = int32(p - 100 + 8)
		case p == 38 || p == 48:
			// Extended colour: 5;n (palette) or 2;r;g;b (truecolour).
			consumed, col := parseExtendedColor(ps[i:])
			if consumed > 0 {
				if p == 38 {
					e.attr.FG = col
				} else {
					e.attr.BG = col
				}
				i += consumed - 1
			}
		}
	}
}

// parseExtendedColor reads a 38/48 sequence, returning how many parameters it
// consumed and the encoded colour.
func parseExtendedColor(ps []int) (int, int32) {
	if len(ps) < 2 {
		return 0, -1
	}
	switch ps[1] {
	case 5:
		if len(ps) < 3 {
			return 0, -1
		}
		return 3, int32(ps[2])
	case 2:
		if len(ps) < 5 {
			return 0, -1
		}
		return 5, int32(ps[2]<<16|ps[3]<<8|ps[4]) | rgbFlag
	}
	return 0, -1
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
