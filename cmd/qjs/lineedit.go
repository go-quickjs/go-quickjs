package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The prompt's line editor.
//
// It reads keys from a terminal in raw mode and draws the line being edited
// with the escape sequences every terminal qjs runs in understands -- a VT100's,
// which Windows' console speaks too once asked to. The line may be longer than
// the terminal is wide: it is drawn over as many rows as it needs, and redrawn
// whole after every key, which is simple and fast enough for a line a person
// types.
//
// It knows nothing of the operating system: the terminal is a reader and a
// writer, which is what lets a test drive it with bytes.

// errInterrupt is Ctrl-C at the prompt.
var errInterrupt = errors.New("interrupt")

// editor edits one line at a time.
type editor struct {
	in  *bufio.Reader
	out io.Writer
	// width reports the terminal's width in columns.
	width func() int
	// complete offers the completions of the word ending at the cursor: the
	// candidates, and where in the line the word starts.
	complete func(line []rune, pos int) (candidates []string, start int)

	history []string
	// histIdx is how far back in history the line shown is, -1 for the line
	// being typed, which pending keeps while history is looked through.
	histIdx int
	pending []rune

	prompt []rune
	line   []rune
	pos    int
	// cursorRow is the row the cursor was drawn on, counted from the row the
	// prompt starts on, which is where the next drawing starts from.
	cursorRow int
	// lastTab records that the key before was a Tab that completed nothing,
	// so that another lists the candidates.
	lastTab bool
	// pasting is set inside a bracketed paste, where a key is its text.
	pasting bool
}

func newEditor(in io.Reader, out io.Writer, width func() int) *editor {
	return &editor{in: bufio.NewReader(in), out: out, width: width, histIdx: -1}
}

// addHistory records a line, unless it is empty or the same as the last.
func (e *editor) addHistory(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == line {
		return
	}
	e.history = append(e.history, line)
	if len(e.history) > historyLimit {
		e.history = e.history[len(e.history)-historyLimit:]
	}
}

// historyLimit is how many lines are remembered.
const historyLimit = 1000

// readLine reads a line with prompt. It returns io.EOF for Ctrl-D on an empty
// line, and errInterrupt for Ctrl-C.
func (e *editor) readLine(prompt string) (string, error) {
	e.prompt, e.line, e.pos = []rune(prompt), e.line[:0], 0
	e.histIdx, e.pending, e.cursorRow, e.lastTab = -1, nil, 0, false
	e.refresh()
	for {
		k, err := e.readKey()
		if err != nil {
			e.finish()
			if err == io.EOF && len(e.line) > 0 {
				return string(e.line), nil
			}
			return "", err
		}
		if k.kind != keyTab {
			e.lastTab = false
		}
		switch k.kind {
		case keyRune:
			e.insert(k.r)
		case keyEnter:
			e.finish()
			return string(e.line), nil
		case keyInterrupt:
			e.pos = len(e.line)
			e.refresh()
			fmt.Fprint(e.out, "^C")
			e.finish()
			return string(e.line), errInterrupt
		case keyEOF:
			if len(e.line) == 0 {
				e.finish()
				return "", io.EOF
			}
			e.deleteAt(e.pos)
		case keyBackspace:
			if e.pos > 0 {
				e.pos--
				e.deleteAt(e.pos)
			}
		case keyDelete:
			e.deleteAt(e.pos)
		case keyLeft:
			if e.pos > 0 {
				e.pos--
			}
		case keyRight:
			if e.pos < len(e.line) {
				e.pos++
			}
		case keyHome:
			e.pos = 0
		case keyEnd:
			e.pos = len(e.line)
		case keyWordLeft:
			e.pos = e.wordStart(e.pos)
		case keyWordRight:
			e.pos = e.wordEnd(e.pos)
		case keyKillEnd:
			e.line = e.line[:e.pos]
		case keyKillStart:
			e.line = append(e.line[:0], e.line[e.pos:]...)
			e.pos = 0
		case keyKillWordBack:
			start := e.wordStart(e.pos)
			e.line = append(e.line[:start], e.line[e.pos:]...)
			e.pos = start
		case keyKillWordForward:
			end := e.wordEnd(e.pos)
			e.line = append(e.line[:e.pos], e.line[end:]...)
		case keyTranspose:
			if e.pos > 0 && len(e.line) > 1 {
				if e.pos == len(e.line) {
					e.pos--
				}
				e.line[e.pos-1], e.line[e.pos] = e.line[e.pos], e.line[e.pos-1]
				e.pos++
			}
		case keyUp:
			e.historyMove(1)
		case keyDown:
			e.historyMove(-1)
		case keyClear:
			fmt.Fprint(e.out, "\x1b[H\x1b[2J")
			e.cursorRow = 0
		case keyTab:
			e.completeWord()
		}
		e.refresh()
	}
}

// insert puts a character at the cursor.
func (e *editor) insert(r rune) {
	e.line = append(e.line, 0)
	copy(e.line[e.pos+1:], e.line[e.pos:])
	e.line[e.pos] = r
	e.pos++
}

// deleteAt removes the character at i, if there is one.
func (e *editor) deleteAt(i int) {
	if i < len(e.line) {
		e.line = append(e.line[:i], e.line[i+1:]...)
	}
}

// isWord reports whether a character is part of a word, for the word motions:
// an identifier's, as the language has them.
func isWord(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// wordStart is where the word before i starts, skipping what is between.
func (e *editor) wordStart(i int) int {
	for i > 0 && !isWord(e.line[i-1]) {
		i--
	}
	for i > 0 && isWord(e.line[i-1]) {
		i--
	}
	return i
}

// wordEnd is where the word after i ends, skipping what is between.
func (e *editor) wordEnd(i int) int {
	for i < len(e.line) && !isWord(e.line[i]) {
		i++
	}
	for i < len(e.line) && isWord(e.line[i]) {
		i++
	}
	return i
}

// historyMove shows the line n further back in history, or forward for a
// negative n, keeping what was being typed to come back to.
func (e *editor) historyMove(n int) {
	idx := e.histIdx + n
	if idx < -1 || idx >= len(e.history) {
		return
	}
	if e.histIdx == -1 {
		e.pending = append(e.pending[:0], e.line...)
	}
	e.histIdx = idx
	if idx == -1 {
		e.line = append(e.line[:0], e.pending...)
	} else {
		e.line = append(e.line[:0], []rune(e.history[len(e.history)-1-idx])...)
	}
	e.pos = len(e.line)
}

// completeWord completes the word before the cursor: as far as every
// candidate agrees, and a second Tab that finds nothing to add lists them.
func (e *editor) completeWord() {
	if e.complete == nil {
		return
	}
	candidates, start := e.complete(e.line, e.pos)
	if len(candidates) == 0 {
		return
	}
	word := string(e.line[start:e.pos])
	common := candidates[0]
	for _, c := range candidates[1:] {
		common = commonPrefix(common, c)
	}
	if len(common) > len(word) {
		add := []rune(common[len(word):])
		e.line = append(e.line[:e.pos], append(add, e.line[e.pos:]...)...)
		e.pos += len(add)
		e.lastTab = false
		return
	}
	if len(candidates) == 1 {
		return
	}
	if !e.lastTab {
		e.lastTab = true
		return
	}
	e.lastTab = false
	e.listCandidates(candidates)
}

func commonPrefix(a, b string) string {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return a[:n]
}

// listCandidates prints completions in columns below the line, and leaves
// the line to be drawn again beneath them.
func (e *editor) listCandidates(candidates []string) {
	sort.Strings(candidates)
	widest := 0
	for _, c := range candidates {
		widest = max(widest, displayWidth([]rune(c)))
	}
	colWidth := widest + 2
	perRow := max(1, e.cols()/colWidth)
	e.pos = len(e.line)
	e.refresh()
	var b strings.Builder
	b.WriteString("\r\n")
	for i, c := range candidates {
		b.WriteString(c)
		if (i+1)%perRow == 0 || i == len(candidates)-1 {
			b.WriteString("\r\n")
		} else {
			b.WriteString(strings.Repeat(" ", colWidth-displayWidth([]rune(c))))
		}
	}
	fmt.Fprint(e.out, b.String())
	e.cursorRow = 0
}

// cols is the terminal's width, never less than one column.
func (e *editor) cols() int {
	if e.width == nil {
		return 80
	}
	return max(1, e.width())
}

// refresh draws the prompt and the line again from the row the prompt starts
// on, and puts the cursor where it belongs.
func (e *editor) refresh() {
	cols := e.cols()
	var b strings.Builder
	// Back to the first row, and clear everything from there down: the old
	// line may have taken more rows than the new one.
	if e.cursorRow > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", e.cursorRow)
	}
	b.WriteString("\r\x1b[J")
	b.WriteString(string(e.prompt))
	b.WriteString(string(e.line))

	promptWidth := displayWidth(e.prompt)
	total := promptWidth + displayWidth(e.line)
	endRow := 0
	if total > 0 {
		endRow = (total - 1) / cols
	}
	// A line that ends exactly at the right edge leaves the cursor waiting to
	// wrap; a line break puts it on the next row, where it belongs.
	if total > 0 && total%cols == 0 && e.pos == len(e.line) {
		b.WriteString("\r\n")
		endRow++
	}
	at := promptWidth + displayWidth(e.line[:e.pos])
	row, col := at/cols, at%cols
	if endRow > row {
		fmt.Fprintf(&b, "\x1b[%dA", endRow-row)
	}
	b.WriteString("\r")
	if col > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", col)
	}
	e.cursorRow = row
	fmt.Fprint(e.out, b.String())
}

// finish leaves the cursor below the line, where what follows is written.
func (e *editor) finish() {
	e.pos = len(e.line)
	e.refresh()
	fmt.Fprint(e.out, "\r\n")
	e.cursorRow = 0
}

// displayWidth is how many columns a terminal gives text: two for a wide
// character, none for a combining mark.
func displayWidth(rs []rune) int {
	w := 0
	for _, r := range rs {
		switch {
		case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), r == 0x200B:
		case isWide(r):
			w += 2
		default:
			w++
		}
	}
	return w
}

// isWide reports whether a terminal draws a character two columns wide: the
// East Asian wide and fullwidth ranges, and the emoji most fonts draw wide.
func isWide(r rune) bool {
	return r >= 0x1100 && (r <= 0x115F ||
		r >= 0x2E80 && r <= 0xA4CF && r != 0x303F ||
		r >= 0xAC00 && r <= 0xD7A3 ||
		r >= 0xF900 && r <= 0xFAFF ||
		r >= 0xFE30 && r <= 0xFE4F ||
		r >= 0xFF00 && r <= 0xFF60 ||
		r >= 0xFFE0 && r <= 0xFFE6 ||
		r >= 0x1F300 && r <= 0x1F64F ||
		r >= 0x1F900 && r <= 0x1F9FF ||
		r >= 0x20000 && r <= 0x3FFFD)
}

// --- Keys ------------------------------------------------------------------

type keyKind int

const (
	keyRune keyKind = iota
	keyEnter
	keyInterrupt
	keyEOF
	keyBackspace
	keyDelete
	keyLeft
	keyRight
	keyHome
	keyEnd
	keyWordLeft
	keyWordRight
	keyKillEnd
	keyKillStart
	keyKillWordBack
	keyKillWordForward
	keyTranspose
	keyUp
	keyDown
	keyClear
	keyTab
	keyNone
)

type key struct {
	kind keyKind
	r    rune
}

// readKey reads one key: a character, a control key, or an escape sequence.
func (e *editor) readKey() (key, error) {
	for {
		r, _, err := e.in.ReadRune()
		if err != nil {
			return key{}, err
		}
		if e.pasting {
			if r == 0x1b {
				k, err := e.readEscape()
				if err != nil {
					return key{}, err
				}
				if k.kind == keyNone {
					continue
				}
				return k, nil
			}
			switch r {
			case '\r', '\n':
				// A line of a pasted block ends as a typed one does.
				if r == '\r' {
					e.skipLF()
				}
				return key{kind: keyEnter}, nil
			case '\t':
				return key{kind: keyRune, r: ' '}, nil
			}
			return key{kind: keyRune, r: r}, nil
		}
		switch r {
		case '\r':
			e.skipLF()
			return key{kind: keyEnter}, nil
		case '\n':
			return key{kind: keyEnter}, nil
		case 3:
			return key{kind: keyInterrupt}, nil
		case 4:
			return key{kind: keyEOF}, nil
		case 127, 8:
			return key{kind: keyBackspace}, nil
		case 1:
			return key{kind: keyHome}, nil
		case 5:
			return key{kind: keyEnd}, nil
		case 2:
			return key{kind: keyLeft}, nil
		case 6:
			return key{kind: keyRight}, nil
		case 11:
			return key{kind: keyKillEnd}, nil
		case 21:
			return key{kind: keyKillStart}, nil
		case 23:
			return key{kind: keyKillWordBack}, nil
		case 20:
			return key{kind: keyTranspose}, nil
		case 16:
			return key{kind: keyUp}, nil
		case 14:
			return key{kind: keyDown}, nil
		case 12:
			return key{kind: keyClear}, nil
		case '\t':
			return key{kind: keyTab}, nil
		case 0x1b:
			k, err := e.readEscape()
			if err != nil {
				return key{}, err
			}
			if k.kind == keyNone {
				continue
			}
			return k, nil
		}
		if r < 0x20 || r == utf8.RuneError {
			continue
		}
		return key{kind: keyRune, r: r}, nil
	}
}

// skipLF consumes the LF of a CR LF, when it has already arrived.
func (e *editor) skipLF() {
	if e.in.Buffered() > 0 {
		if b, err := e.in.Peek(1); err == nil && b[0] == '\n' {
			e.in.ReadByte()
		}
	}
}

// readEscape reads what follows an escape: a CSI or SS3 sequence, or an Alt
// key. One it does not know is read to its end and ignored.
func (e *editor) readEscape() (key, error) {
	b, err := e.in.ReadByte()
	if err != nil {
		return key{}, err
	}
	switch b {
	case '[':
		return e.readCSI()
	case 'O':
		c, err := e.in.ReadByte()
		if err != nil {
			return key{}, err
		}
		switch c {
		case 'H':
			return key{kind: keyHome}, nil
		case 'F':
			return key{kind: keyEnd}, nil
		case 'A':
			return key{kind: keyUp}, nil
		case 'B':
			return key{kind: keyDown}, nil
		case 'C':
			return key{kind: keyRight}, nil
		case 'D':
			return key{kind: keyLeft}, nil
		}
		return key{kind: keyNone}, nil
	case 'b', 'B':
		return key{kind: keyWordLeft}, nil
	case 'f', 'F':
		return key{kind: keyWordRight}, nil
	case 'd', 'D':
		return key{kind: keyKillWordForward}, nil
	case 127, 8:
		return key{kind: keyKillWordBack}, nil
	}
	return key{kind: keyNone}, nil
}

// readCSI reads a control sequence after ESC [: parameters, then the final
// byte that says what it is.
func (e *editor) readCSI() (key, error) {
	var params []byte
	for {
		c, err := e.in.ReadByte()
		if err != nil {
			return key{}, err
		}
		if c >= 0x40 && c <= 0x7e {
			return e.csiKey(string(params), c), nil
		}
		params = append(params, c)
		if len(params) > 16 {
			return key{kind: keyNone}, nil
		}
	}
}

// csiKey names a control sequence by its parameters and final byte.
func (e *editor) csiKey(params string, final byte) key {
	// A modifier after the semicolon: 3 is Alt, 5 is Ctrl, which on an arrow
	// moves by word.
	mod := ""
	if i := strings.IndexByte(params, ';'); i >= 0 {
		mod = params[i+1:]
		params = params[:i]
	}
	byWord := mod == "3" || mod == "5" || mod == "9"
	switch final {
	case 'A':
		return key{kind: keyUp}
	case 'B':
		return key{kind: keyDown}
	case 'C':
		if byWord {
			return key{kind: keyWordRight}
		}
		return key{kind: keyRight}
	case 'D':
		if byWord {
			return key{kind: keyWordLeft}
		}
		return key{kind: keyLeft}
	case 'H':
		return key{kind: keyHome}
	case 'F':
		return key{kind: keyEnd}
	case '~':
		switch params {
		case "1", "7":
			return key{kind: keyHome}
		case "4", "8":
			return key{kind: keyEnd}
		case "3":
			if mod == "5" {
				return key{kind: keyKillWordForward}
			}
			return key{kind: keyDelete}
		case "200":
			e.pasting = true
			return key{kind: keyNone}
		case "201":
			e.pasting = false
			return key{kind: keyNone}
		}
	}
	return key{kind: keyNone}
}
