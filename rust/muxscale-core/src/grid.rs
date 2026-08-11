//! The client-side grid model.
//!
//! This is the second implementation of a wire format the Go daemon also
//! implements. Two implementations drift apart silently, and the symptom is a
//! phone rendering something the daemon never had -- close to impossible to
//! diagnose from a bug report. So this file is deliberately a *mirror* of
//! `go/internal/vt/grid.go`, and `tests/cross_impl.rs` replays a fixture the Go
//! side generated to prove they still agree.
//!
//! The client never parses ANSI. It receives snapshots and row deltas and
//! applies them, which is why a lost delta is repaired by asking for a fresh
//! snapshot rather than corrupting the display forever.

use serde::{Deserialize, Deserializer, Serialize};

/// Deserialises a sequence, accepting `null` as empty.
///
/// Go marshals a nil slice as `null`, not `[]`, and serde's `default` attribute
/// only fires when a field is *absent* -- not when it is present and null. So a
/// row with no runs, or a delta with no changed lines, fails to parse without
/// this. It is the first thing the cross-implementation fixture caught, and the
/// kind of mismatch that only shows up between languages.
fn null_as_empty<'de, D, T>(d: D) -> Result<Vec<T>, D::Error>
where
    D: Deserializer<'de>,
    T: Deserialize<'de>,
{
    Ok(Option::<Vec<T>>::deserialize(d)?.unwrap_or_default())
}

/// Marks a colour as 24-bit rather than a palette index.
pub const RGB_FLAG: i32 = 1 << 24;

/// A cell's rendition.
///
/// Field names match the Go struct's JSON exactly. Go's `encoding/json`
/// capitalises exported fields by default, which is why these are `FG`/`Bold`
/// rather than snake_case: matching the wire matters more than matching Rust
/// convention.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Attr {
    #[serde(rename = "FG", default = "default_colour")]
    pub fg: i32,
    #[serde(rename = "BG", default = "default_colour")]
    pub bg: i32,
    #[serde(rename = "Bold", default)]
    pub bold: bool,
    #[serde(rename = "Italic", default)]
    pub italic: bool,
    #[serde(rename = "Underline", default)]
    pub underline: bool,
    #[serde(rename = "Reverse", default)]
    pub reverse: bool,
    #[serde(rename = "Dim", default)]
    pub dim: bool,
    #[serde(rename = "Strike", default)]
    pub strike: bool,
}

fn default_colour() -> i32 {
    -1
}

/// Extracts r, g, b from an encoded truecolour value.
fn rgb_of(v: i32, is_rgb: bool) -> Option<(u8, u8, u8)> {
    if !is_rgb {
        return None;
    }
    let c = v & 0xff_ffff;
    Some(((c >> 16) as u8, (c >> 8) as u8, c as u8))
}

impl Default for Attr {
    fn default() -> Self {
        Attr {
            fg: -1,
            bg: -1,
            bold: false,
            italic: false,
            underline: false,
            reverse: false,
            dim: false,
            strike: false,
        }
    }
}

impl Attr {
    /// True when this is the reset rendition.
    pub fn is_default(&self) -> bool {
        *self == Attr::default()
    }

    /// True when the foreground is 24-bit rather than a palette index.
    ///
    /// The `>= 0` guard is load-bearing. The default colour is -1, and in two's
    /// complement `-1 & RGB_FLAG` is non-zero, so a bare flag test reports every
    /// default cell as truecolour white -- which is to say, the entire terminal
    /// renders in the wrong colour. Found by a round-trip test.
    pub fn fg_is_rgb(&self) -> bool {
        self.fg >= 0 && self.fg & RGB_FLAG != 0
    }

    /// True when the background is 24-bit rather than a palette index.
    pub fn bg_is_rgb(&self) -> bool {
        self.bg >= 0 && self.bg & RGB_FLAG != 0
    }

    /// Returns the r, g, b components of a truecolour foreground.
    pub fn fg_rgb(&self) -> Option<(u8, u8, u8)> {
        rgb_of(self.fg, self.fg_is_rgb())
    }

    /// Returns the r, g, b components of a truecolour background.
    pub fn bg_rgb(&self) -> Option<(u8, u8, u8)> {
        rgb_of(self.bg, self.bg_is_rgb())
    }

    /// Returns the palette index, if the colour is one.
    ///
    /// A renderer needs all three cases distinguished -- default, palette,
    /// truecolour -- because they map to different theme lookups.
    pub fn fg_palette(&self) -> Option<u8> {
        if self.fg < 0 || self.fg_is_rgb() || self.fg > 255 {
            return None;
        }
        Some(self.fg as u8)
    }

    /// Returns the background palette index, if the colour is one.
    pub fn bg_palette(&self) -> Option<u8> {
        if self.bg < 0 || self.bg_is_rgb() || self.bg > 255 {
            return None;
        }
        Some(self.bg as u8)
    }
}

/// One screen position.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Cell {
    pub ch: char,
    pub attr: Attr,
    /// The second column of a double-width character, holding no rune of its
    /// own. Without this, CJK text drifts by one column per glyph.
    pub wide: bool,
}

impl Default for Cell {
    fn default() -> Self {
        Cell {
            ch: ' ',
            attr: Attr::default(),
            wide: false,
        }
    }
}

/// A horizontal span of cells sharing one rendition.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Run {
    #[serde(rename = "t")]
    pub text: String,
    #[serde(rename = "a")]
    pub attr: Attr,
}

/// One row encoded as runs.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RowRuns {
    #[serde(rename = "y")]
    pub y: usize,
    #[serde(rename = "r", default, deserialize_with = "null_as_empty")]
    pub runs: Vec<Run>,
}

/// A full screen state.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Snapshot {
    pub cols: usize,
    pub rows: usize,
    #[serde(rename = "cx")]
    pub cursor_x: usize,
    #[serde(rename = "cy")]
    pub cursor_y: usize,
    #[serde(rename = "coff", default)]
    pub cursor_off: bool,
    #[serde(default, deserialize_with = "null_as_empty")]
    pub lines: Vec<RowRuns>,
}

/// The rows that changed since a baseline.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Delta {
    #[serde(default)]
    pub cols: usize,
    #[serde(default)]
    pub rows: usize,
    #[serde(rename = "cx", default)]
    pub cursor_x: usize,
    #[serde(rename = "cy", default)]
    pub cursor_y: usize,
    #[serde(rename = "coff", default)]
    pub cursor_off: bool,
    #[serde(default, deserialize_with = "null_as_empty")]
    pub lines: Vec<RowRuns>,
}

/// A screen buffer.
#[derive(Debug, Clone)]
pub struct Grid {
    pub cols: usize,
    pub rows: usize,
    cells: Vec<Cell>,
    pub cursor_x: usize,
    pub cursor_y: usize,
    pub cursor_off: bool,
}

impl Grid {
    /// Builds a blank grid.
    pub fn new(cols: usize, rows: usize) -> Self {
        let cols = if cols == 0 { 80 } else { cols };
        let rows = if rows == 0 { 24 } else { rows };
        Grid {
            cols,
            rows,
            cells: vec![Cell::default(); cols * rows],
            cursor_x: 0,
            cursor_y: 0,
            cursor_off: false,
        }
    }

    /// Returns the cell at (x, y), or a blank for out-of-range coordinates.
    ///
    /// Out-of-range reads return blanks rather than panicking: a renderer
    /// racing a resize would otherwise crash the app over a cosmetic issue.
    pub fn at(&self, x: usize, y: usize) -> Cell {
        if x >= self.cols || y >= self.rows {
            return Cell::default();
        }
        self.cells[y * self.cols + x]
    }

    /// Writes a cell, ignoring out-of-range coordinates.
    pub fn set(&mut self, x: usize, y: usize, c: Cell) {
        if x >= self.cols || y >= self.rows {
            return;
        }
        self.cells[y * self.cols + x] = c;
    }

    /// Renders a row as text with trailing blanks trimmed.
    pub fn row_text(&self, y: usize) -> String {
        if y >= self.rows {
            return String::new();
        }
        let mut s = String::with_capacity(self.cols);
        for x in 0..self.cols {
            let c = self.cells[y * self.cols + x];
            if c.wide {
                continue; // the wide rune was emitted by its first column
            }
            s.push(c.ch);
        }
        while s.ends_with(' ') {
            s.pop();
        }
        s
    }

    /// Renders the whole grid, trailing blank lines trimmed.
    pub fn text(&self) -> String {
        let mut lines: Vec<String> = (0..self.rows).map(|y| self.row_text(y)).collect();
        while lines.last().map(|l| l.is_empty()).unwrap_or(false) {
            lines.pop();
        }
        lines.join("\n")
    }

    /// Changes dimensions, preserving overlapping content.
    pub fn resize(&mut self, cols: usize, rows: usize) {
        if cols == 0 || rows == 0 || (cols == self.cols && rows == self.rows) {
            return;
        }
        let mut next = vec![Cell::default(); cols * rows];
        for y in 0..rows.min(self.rows) {
            for x in 0..cols.min(self.cols) {
                next[y * cols + x] = self.cells[y * self.cols + x];
            }
        }
        self.cells = next;
        self.cols = cols;
        self.rows = rows;
        self.cursor_x = self.cursor_x.min(cols.saturating_sub(1));
        self.cursor_y = self.cursor_y.min(rows.saturating_sub(1));
    }

    /// Rebuilds the grid from a snapshot.
    pub fn apply_snapshot(&mut self, s: &Snapshot) {
        if s.cols != self.cols || s.rows != self.rows {
            *self = Grid::new(s.cols, s.rows);
        } else {
            self.cells.iter_mut().for_each(|c| *c = Cell::default());
        }
        for line in &s.lines {
            self.apply_runs(line);
        }
        self.cursor_x = s.cursor_x;
        self.cursor_y = s.cursor_y;
        self.cursor_off = s.cursor_off;
    }

    /// Applies changed rows.
    ///
    /// A dimension change rebuilds: applying a partial update against a
    /// differently-sized grid is how corruption starts.
    pub fn apply_delta(&mut self, d: &Delta) {
        if d.cols != 0 && d.rows != 0 && (d.cols != self.cols || d.rows != self.rows) {
            self.resize(d.cols, d.rows);
        }
        for line in &d.lines {
            self.apply_runs(line);
        }
        self.cursor_x = d.cursor_x;
        self.cursor_y = d.cursor_y;
        self.cursor_off = d.cursor_off;
    }

    /// Writes one encoded row, padding the remainder with blanks.
    ///
    /// The padding is what makes trailing-blank trimming safe on the sender:
    /// the daemon omits trailing default-rendition spaces, and this fills them
    /// back in.
    fn apply_runs(&mut self, line: &RowRuns) {
        if line.y >= self.rows {
            return;
        }
        let mut x = 0usize;
        for run in &line.runs {
            for ch in run.text.chars() {
                if x >= self.cols {
                    break;
                }
                self.set(
                    x,
                    line.y,
                    Cell {
                        ch,
                        attr: run.attr,
                        wide: false,
                    },
                );
                x += 1;
                if rune_width(ch) == 2 && x < self.cols {
                    self.set(
                        x,
                        line.y,
                        Cell {
                            ch: '\0',
                            attr: run.attr,
                            wide: true,
                        },
                    );
                    x += 1;
                }
            }
        }
        while x < self.cols {
            self.set(x, line.y, Cell::default());
            x += 1;
        }
    }
}

/// Display columns for a character: 0 for combining marks, 2 for wide CJK and
/// emoji, else 1.
///
/// This must agree exactly with the Go implementation. A disagreement shifts
/// every subsequent column on the row, which is precisely the kind of drift the
/// cross-implementation fixture exists to catch.
pub fn rune_width(ch: char) -> usize {
    let c = ch as u32;
    if c == 0 {
        return 0;
    }
    if c < 0x20 || (0x7f..0xa0).contains(&c) {
        return 0;
    }
    if is_combining(c) {
        return 0;
    }
    if is_wide(c) {
        return 2;
    }
    1
}

fn is_combining(c: u32) -> bool {
    (0x0300..=0x036f).contains(&c)
        || (0x1ab0..=0x1aff).contains(&c)
        || (0x20d0..=0x20ff).contains(&c)
        || (0xfe00..=0xfe0f).contains(&c) // variation selectors
        || (0xfe20..=0xfe2f).contains(&c)
}

fn is_wide(c: u32) -> bool {
    (0x1100..=0x115f).contains(&c)      // Hangul Jamo
        || (0x2e80..=0x303e).contains(&c)   // CJK radicals, Kangxi
        || (0x3041..=0x33ff).contains(&c)   // Hiragana .. CJK compat
        || (0x3400..=0x4dbf).contains(&c)   // CJK ext A
        || (0x4e00..=0x9fff).contains(&c)   // CJK unified
        || (0xa000..=0xa4cf).contains(&c)   // Yi
        || (0xac00..=0xd7a3).contains(&c)   // Hangul syllables
        || (0xf900..=0xfaff).contains(&c)   // CJK compat ideographs
        || (0xfe30..=0xfe6f).contains(&c)   // CJK compat forms
        || (0xff00..=0xff60).contains(&c)   // fullwidth forms
        || (0xffe0..=0xffe6).contains(&c)
        || (0x1f300..=0x1f64f).contains(&c) // emoji
        || (0x1f900..=0x1f9ff).contains(&c)
        || (0x20000..=0x3fffd).contains(&c)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(text: &str) -> Run {
        Run {
            text: text.into(),
            attr: Attr::default(),
        }
    }

    #[test]
    fn blank_grid_is_spaces() {
        let g = Grid::new(4, 2);
        assert_eq!(g.text(), "");
        assert_eq!(g.at(0, 0).ch, ' ');
    }

    #[test]
    fn apply_runs_pads_the_remainder() {
        let mut g = Grid::new(8, 2);
        g.apply_runs(&RowRuns {
            y: 0,
            runs: vec![run("hi")],
        });
        assert_eq!(g.row_text(0), "hi");
        // The daemon trims trailing blanks, so the client must fill them in.
        assert_eq!(g.at(7, 0).ch, ' ');
    }

    #[test]
    fn out_of_range_access_does_not_panic() {
        let mut g = Grid::new(4, 2);
        // A renderer racing a resize must not crash the app over a cosmetic
        // issue.
        assert_eq!(g.at(99, 99).ch, ' ');
        g.set(99, 99, Cell::default());
        assert_eq!(g.row_text(99), "");
    }

    #[test]
    fn wide_runes_occupy_two_columns() {
        assert_eq!(rune_width('日'), 2);
        assert_eq!(rune_width('a'), 1);
        assert_eq!(rune_width('\u{0301}'), 0); // combining acute

        let mut g = Grid::new(6, 1);
        g.apply_runs(&RowRuns {
            y: 0,
            runs: vec![run("日本")],
        });
        assert!(g.at(1, 0).wide, "column 1 continues the first wide rune");
        assert!(g.at(3, 0).wide, "column 3 continues the second");
        assert_eq!(g.row_text(0), "日本");
    }

    #[test]
    fn resize_preserves_overlap() {
        let mut g = Grid::new(4, 2);
        g.apply_runs(&RowRuns {
            y: 0,
            runs: vec![run("abcd")],
        });
        g.resize(8, 4);
        assert_eq!(g.row_text(0), "abcd");
        assert_eq!(g.cols, 8);
    }

    #[test]
    fn truecolour_round_trips() {
        let a = Attr {
            fg: (10 << 16 | 20 << 8 | 30) | RGB_FLAG,
            bg: (1 << 16 | 2 << 8 | 3) | RGB_FLAG,
            ..Attr::default()
        };
        assert!(a.fg_is_rgb());
        assert_eq!(a.fg_rgb(), Some((10, 20, 30)));
        assert_eq!(a.bg_rgb(), Some((1, 2, 3)));
        assert_eq!(
            a.fg_palette(),
            None,
            "a truecolour value is not a palette index"
        );
    }

    /// Regression: the default colour is -1, and in two's complement
    /// `-1 & RGB_FLAG` is non-zero. A bare flag test reported every default
    /// cell as truecolour white, i.e. the whole terminal in the wrong colour.
    #[test]
    fn default_colour_is_not_mistaken_for_truecolour() {
        let a = Attr::default();
        assert!(!a.fg_is_rgb(), "-1 must not read as truecolour");
        assert!(!a.bg_is_rgb());
        assert_eq!(a.fg_rgb(), None);
        assert_eq!(a.bg_rgb(), None);
        assert_eq!(a.fg_palette(), None, "default is not palette 0 either");
    }

    /// A renderer needs default, palette and truecolour distinguished, because
    /// each maps to a different theme lookup.
    #[test]
    fn three_colour_cases_are_distinguishable() {
        let palette = Attr {
            fg: 208,
            ..Attr::default()
        };
        assert_eq!(palette.fg_palette(), Some(208));
        assert!(!palette.fg_is_rgb());

        let basic = Attr {
            fg: 2,
            ..Attr::default()
        };
        assert_eq!(basic.fg_palette(), Some(2));

        let truecolour = Attr {
            fg: 0x0a141e | RGB_FLAG,
            ..Attr::default()
        };
        assert_eq!(truecolour.fg_palette(), None);
        assert!(truecolour.fg_is_rgb());
    }

    /// Regression: Go marshals a nil slice as `null`, and serde's `default`
    /// only fires for an absent field. A row with no runs, or a delta with no
    /// changed lines, failed to parse at all.
    #[test]
    fn null_sequences_from_go_parse_as_empty() {
        let row: RowRuns = serde_json::from_str(r#"{"y":3,"r":null}"#).unwrap();
        assert_eq!(row.y, 3);
        assert!(row.runs.is_empty());

        let snap: Snapshot =
            serde_json::from_str(r#"{"cols":4,"rows":2,"cx":0,"cy":0,"lines":null}"#).unwrap();
        assert!(snap.lines.is_empty());

        let delta: Delta = serde_json::from_str(r#"{"cols":4,"rows":2,"lines":null}"#).unwrap();
        assert!(delta.lines.is_empty());

        // An absent field must still work.
        let bare: RowRuns = serde_json::from_str(r#"{"y":1}"#).unwrap();
        assert!(bare.runs.is_empty());
    }

    #[test]
    fn default_attr_matches_the_go_zero_value() {
        // Go's DefaultAttr is {FG: -1, BG: -1}. A mismatch here would render
        // every default cell in palette colour 0 instead of the theme default.
        let a = Attr::default();
        assert_eq!(a.fg, -1);
        assert_eq!(a.bg, -1);
        assert!(a.is_default());
    }
}
