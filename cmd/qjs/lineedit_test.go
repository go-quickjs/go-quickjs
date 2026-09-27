package main

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"regexp"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// edit feeds keys to an editor and returns the lines it reads, each followed
// by the error it ended with when that was not a plain Enter.
func edit(t *testing.T, e *editor, keys string) []string {
	t.Helper()
	e.in.Reset(strings.NewReader(keys))
	var got []string
	for {
		line, err := e.readLine("> ")
		switch {
		case err == io.EOF:
			return append(got, "<EOF>")
		case errors.Is(err, errInterrupt):
			got = append(got, line+"<^C>")
		case err != nil:
			t.Fatal(err)
		default:
			got = append(got, line)
		}
	}
}

func newTestEditor(width int) (*editor, *bytes.Buffer) {
	var out bytes.Buffer
	return newEditor(strings.NewReader(""), &out, func() int { return width }), &out
}

// TestEditorKeys pins what each key does to the line.
func TestEditorKeys(t *testing.T) {
	const (
		left, right = "\x1b[D", "\x1b[C"
		home, end   = "\x1b[H", "\x1b[F"
	)
	for _, tc := range []struct{ name, keys, want string }{
		{"typing", "let x = 1\r", "let x = 1"},
		{"left and insert", "hello" + left + left + "X\r", "helXlo"},
		{"right", "ab" + left + left + right + "X\r", "aXb"},
		{"home and end", "bc" + home + "a" + end + "d\r", "abcd"},
		{"ctrl-a and ctrl-e", "bc\x01a\x05d\r", "abcd"},
		{"ctrl-b and ctrl-f", "ac\x02\x02\x06b\r", "abc"},
		{"ss3 and tilde home and end", "b\x1bOHa\x1b[4~c\x1b[1~_\r", "_abc"},
		{"backspace", "abcd\x7f\x7f\r", "ab"},
		{"backspace in the middle", "abcd" + left + "\x7f\r", "abd"},
		{"delete", "abcd" + home + "\x1b[3~\r", "bcd"},
		{"ctrl-d deletes in a line", "abc" + home + "\x04\r", "bc"},
		{"ctrl-k", "hello world" + home + "\x1bf\x0b\r", "hello"},
		{"ctrl-u", "hello world\x1bb\x15\r", "world"},
		{"ctrl-w", "foo bar baz\x17\r", "foo bar "},
		{"alt-backspace", "foo.bar\x1b\x7f\r", "foo."},
		{"alt-d", "foo bar" + home + "\x1bd\r", " bar"},
		{"ctrl-t", "ab\x14\r", "ba"},
		{"ctrl-left and ctrl-right", "one two three\x1b[1;5D\x1b[1;5DX\x1b[1;5CY\r", "one XtwoY three"},
		{"alt-left", "one two\x1b[1;3DX\r", "one Xtwo"},
		{"alt-b and alt-f", "one two\x1bb\x1bb\x1bfX\r", "oneX two"},
		{"unicode", "日本" + left + "語\r", "日語本"},
		{"crlf is one enter", "a\r\nb\r", "a"},
		{"unknown sequences are ignored", "a\x1b[5~\x1b[99;9zb\r", "ab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestEditor(80)
			if got := edit(t, e, tc.keys); got[0] != tc.want {
				t.Errorf("line = %q, want %q", got[0], tc.want)
			}
		})
	}
}

// TestEditorControl pins Ctrl-C, Ctrl-D and the end of input.
func TestEditorControl(t *testing.T) {
	e, _ := newTestEditor(80)
	got := edit(t, e, "abc\x03\x03x\r\x04")
	want := []string{"abc<^C>", "<^C>", "x", "<EOF>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// Input that ends in the middle of a line gives the line.
	e, _ = newTestEditor(80)
	if got := edit(t, e, "partial"); !reflect.DeepEqual(got, []string{"partial", "<EOF>"}) {
		t.Errorf("got %q", got)
	}
}

// TestEditorHistory pins moving through history and back to the line being
// typed.
func TestEditorHistory(t *testing.T) {
	e, _ := newTestEditor(80)
	for _, l := range []string{"one", "two", "two", "", "three"} {
		e.addHistory(l)
	}
	if !reflect.DeepEqual(e.history, []string{"one", "two", "three"}) {
		t.Fatalf("history = %q", e.history)
	}
	const up, down = "\x1b[A", "\x1b[B"
	got := edit(t, e, up+"\r"+up+up+up+up+"\r"+"typed"+up+up+down+down+"!\r"+"\x10\x10\x0e\r")
	want := []string{"three", "one", "typed!", "three", "<EOF>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEditorCompletion pins Tab: completing as far as the candidates agree,
// and listing them on a second Tab that adds nothing.
func TestEditorCompletion(t *testing.T) {
	e, out := newTestEditor(80)
	e.complete = func(line []rune, pos int) ([]string, int) {
		start := pos
		for start > 0 && isWord(line[start-1]) {
			start--
		}
		var out []string
		for _, c := range []string{"Array", "ArrayBuffer", "Atomics", "Boolean"} {
			if strings.HasPrefix(c, string(line[start:pos])) {
				out = append(out, c)
			}
		}
		return out, start
	}
	got := edit(t, e, "Bo\t\r"+"Arr\t\r"+"new Arr\t\t\t\r")
	want := []string{"Boolean", "Array", "new Array", "<EOF>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if !regexp.MustCompile(`\r\nArray +ArrayBuffer\r\n`).MatchString(out.String()) {
		t.Errorf("the candidates were not listed:\n%q", out.String())
	}
}

// TestEditorPaste pins a bracketed paste: its text is inserted as it is, and
// each line of it ends as a typed line does.
func TestEditorPaste(t *testing.T) {
	e, _ := newTestEditor(80)
	got := edit(t, e, "x = \x1b[200~[1,\n\t2]\x1b[201~;\r")
	want := []string{"x = [1,", " 2];", "<EOF>"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEditorRendering pins how a line is drawn: over as many rows as it
// needs, with the cursor put back where it belongs.
func TestEditorRendering(t *testing.T) {
	e, out := newTestEditor(10)
	e.prompt, e.line = []rune("> "), []rune("0123456789ab")
	e.pos = 3
	e.refresh()
	// 14 columns on a 10-column terminal: the cursor is on the first row,
	// having gone up from the second, five columns in.
	if got, want := out.String(), "\r\x1b[J> 0123456789ab\x1b[1A\r\x1b[5C"; got != want {
		t.Errorf("drawn %q, want %q", got, want)
	}
	if e.cursorRow != 0 {
		t.Errorf("cursorRow = %d", e.cursorRow)
	}
	out.Reset()
	// A line that fills its last row exactly leaves the cursor on the next.
	e.line, e.pos = []rune("01234567"), 8
	e.refresh()
	if got, want := out.String(), "\r\x1b[J> 01234567\r\n\r"; got != want {
		t.Errorf("drawn %q, want %q", got, want)
	}
	if e.cursorRow != 1 {
		t.Errorf("cursorRow = %d", e.cursorRow)
	}
	if w := displayWidth([]rune("a日本́")); w != 5 {
		t.Errorf("displayWidth = %d, want 5", w)
	}
}

// TestCompleter pins what Tab offers: globals and keywords, what the prompt
// declared, and the properties of a chain of names.
func TestCompleter(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	c := &completer{rt: rt, declared: map[string]bool{}}
	if _, err := rt.Eval("let myThing = { alpha: 1, alps: 2, beta: 3 }"); err != nil {
		t.Fatal(err)
	}
	c.noteDeclarations("let myThing = { alpha: 1 }")
	for _, tc := range []struct {
		line  string
		want  []string
		start int
	}{
		{"Math.P", []string{"PI"}, 5},
		{"x = myTh", []string{"myThing"}, 4},
		{"myThing.al", []string{"alpha", "alps"}, 8},
		{"myThing.alpha.toFi", []string{"toFixed"}, 14},
		{"typeo", []string{"typeof"}, 0},
		{"Array.isArr", []string{"isArray"}, 6},
		{"nothing.here.", nil, 13},
		{"(1).", nil, 4},
	} {
		got, start := c.complete([]rune(tc.line), len([]rune(tc.line)))
		if !reflect.DeepEqual(got, tc.want) || start != tc.start {
			t.Errorf("%q: got %q at %d, want %q at %d", tc.line, got, start, tc.want, tc.start)
		}
	}
}
