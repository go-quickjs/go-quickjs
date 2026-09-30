package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// lineSource is where the prompt's lines come from: a terminal, edited as it
// is typed, or anything else read a line at a time.
type lineSource interface {
	// readLine reads a line after showing prompt. It returns io.EOF when the
	// input is over and errInterrupt for Ctrl-C.
	readLine(prompt string) (string, error)
	// remember records a line the prompt ran, for history.
	remember(line string)
	// close gives the terminal back as it was.
	close()
}

// scannedLines reads lines from input that is not a terminal, printing the
// prompt before each.
type scannedLines struct {
	in  *bufio.Scanner
	out io.Writer
}

func newScannedLines(stdin io.Reader, stdout io.Writer) *scannedLines {
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 0, 64*1024), 16<<20)
	return &scannedLines{in: in, out: stdout}
}

func (s *scannedLines) readLine(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	if !s.in.Scan() {
		if err := s.in.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	return s.in.Text(), nil
}

func (s *scannedLines) remember(string) {}
func (s *scannedLines) close()          {}

// editedLines reads lines from a terminal through the line editor, which the
// terminal is in raw mode for only while a line is being read.
type editedLines struct {
	t       *terminal
	ed      *editor
	history string
}

func newEditedLines(t *terminal, complete func([]rune, int) ([]string, int)) *editedLines {
	ed := newEditor(t.in, t.w, t.width)
	ed.complete = complete
	l := &editedLines{t: t, ed: ed, history: historyPath()}
	l.load()
	return l
}

func (l *editedLines) readLine(prompt string) (string, error) {
	if err := l.t.raw(); err != nil {
		return "", err
	}
	defer l.t.restore()
	return l.ed.readLine(prompt)
}

func (l *editedLines) remember(line string) {
	prev := ""
	if n := len(l.ed.history); n > 0 {
		prev = l.ed.history[n-1]
	}
	l.ed.addHistory(line)
	if l.history == "" || strings.TrimSpace(line) == "" || line == prev {
		return
	}
	// Each line is added to the file as it is run, so that a session that
	// ends abruptly keeps what it typed.
	f, err := os.OpenFile(l.history, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func (l *editedLines) close() { l.t.restore() }

// load reads the history file, keeping its last lines -- and writes it back
// shortened when it has grown well past them.
func (l *editedLines) load() {
	if l.history == "" {
		return
	}
	data, err := os.ReadFile(l.history)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for _, line := range lines {
		l.ed.addHistory(strings.TrimRight(line, "\r"))
	}
	if len(lines) > 2*historyLimit {
		os.WriteFile(l.history, []byte(strings.Join(l.ed.history, "\n")+"\n"), 0o600)
	}
}

// historyPath is where the prompt's history is kept: QJS_HISTORY when it is
// set -- to nothing, to keep none -- and otherwise .qjs_history in the home
// directory.
func historyPath() string {
	if p, ok := os.LookupEnv("QJS_HISTORY"); ok {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".qjs_history")
}

// --- Completion --------------------------------------------------------------

// completer offers what can follow the word before the cursor: a global, a
// keyword, a name the prompt has declared, or -- after a chain of names and
// dots -- a property of what the chain names.
type completer struct {
	rt *quickjs.Runtime
	// declared are the let, const and class names the prompt has run, which
	// are globals no property lookup finds.
	declared map[string]bool
}

var (
	identifierRe  = regexp.MustCompile(`^[A-Za-z_$][\w$]*$`)
	declarationRe = regexp.MustCompile(`\b(?:let|const|class|var|function)\s+([A-Za-z_$][\w$]*)`)
	jsKeywords    = strings.Fields(`await break case catch class const continue debugger default delete do
		else export extends false finally for function if import in instanceof let new null of return
		static super switch this throw true try typeof undefined var void while with yield async`)
)

// noteDeclarations remembers the names a line the prompt ran declared.
func (c *completer) noteDeclarations(src string) {
	for _, m := range declarationRe.FindAllStringSubmatch(src, -1) {
		c.declared[m[1]] = true
	}
}

func (c *completer) complete(line []rune, pos int) ([]string, int) {
	start := pos
	for start > 0 && isWord(line[start-1]) {
		start--
	}
	prefix := string(line[start:pos])
	// The chain of names and dots before the word, if there is one.
	chainEnd := start
	for chainEnd > 0 && line[chainEnd-1] == ' ' {
		chainEnd--
	}
	var names []string
	if chainEnd > 0 && line[chainEnd-1] == '.' {
		chainStart := chainEnd - 1
		for chainStart > 0 && (isWord(line[chainStart-1]) || line[chainStart-1] == '.') {
			chainStart--
		}
		chain := string(line[chainStart : chainEnd-1])
		if chain == "" || !validChain(chain) {
			return nil, start
		}
		names = c.propertiesOf(chain)
	} else {
		if prefix == "" {
			return nil, start
		}
		names = c.propertiesOf("globalThis")
		names = append(names, jsKeywords...)
		for n := range c.declared {
			names = append(names, n)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(n, prefix) && identifierRe.MatchString(n) && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, start
}

// validChain reports whether text is names separated by dots, which is all
// completion evaluates: nothing in it can call anything but a getter.
func validChain(chain string) bool {
	for _, part := range strings.Split(chain, ".") {
		if !identifierRe.MatchString(part) {
			return false
		}
	}
	return true
}

// propertiesOf lists the string-keyed properties of what an expression names,
// along its prototype chain. It gives up on an expression that fails, or that
// takes longer than a keypress should.
func (c *completer) propertiesOf(expr string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	v, err := c.rt.EvalContext(ctx, `(() => {
		const names = new Set();
		for (let o = Object(`+expr+`); o !== null; o = Object.getPrototypeOf(o)) {
			for (const k of Object.getOwnPropertyNames(o)) names.add(k);
		}
		return [...names].join("\n");
	})()`)
	if err != nil || v.IsUndefined() {
		return nil
	}
	return strings.Split(v.String(), "\n")
}
