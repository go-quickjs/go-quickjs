package main

import (
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// terminal is standard input and output when both are a terminal the line
// editor can drive.
type terminal struct {
	in, out *os.File
	// w is where the editor writes: out, behind the lock qjs's other writers
	// take.
	w      io.Writer
	cooked *term.State
}

// openTerminal returns the terminal behind stdin and stdout, or nil when they
// are not one -- a pipe, a file, a test -- or it cannot be driven: a Windows
// console too old to understand escape sequences.
func openTerminal(stdin io.Reader, stdout io.Writer) *terminal {
	in, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return nil
	}
	out, ok := fileOf(stdout)
	if !ok || !term.IsTerminal(int(out.Fd())) {
		return nil
	}
	if !enableEscapes(out) {
		return nil
	}
	return &terminal{in: in, out: out, w: stdout}
}

// fileOf is the file a writer writes to, when it is one: the writer itself,
// or the one behind the lock run puts on qjs's output.
func fileOf(w io.Writer) (*os.File, bool) {
	if l, ok := w.(*lockedWriter); ok {
		w = l.w
	}
	f, ok := w.(*os.File)
	return f, ok
}

// raw puts the terminal in raw mode, where each key arrives as it is pressed
// and nothing is echoed, and asks it to mark pasted text.
func (t *terminal) raw() error {
	st, err := term.MakeRaw(int(t.in.Fd()))
	if err != nil {
		return err
	}
	t.cooked = st
	fmt.Fprint(t.w, "\x1b[?2004h")
	return nil
}

// restore puts the terminal back as it was, which is how the code a line runs
// writes to it: with its own line endings, and able to be interrupted.
func (t *terminal) restore() {
	if t.cooked == nil {
		return
	}
	fmt.Fprint(t.w, "\x1b[?2004l")
	term.Restore(int(t.in.Fd()), t.cooked)
	t.cooked = nil
}

// width is the terminal's width in columns.
func (t *terminal) width() int {
	w, _, err := term.GetSize(int(t.out.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}
