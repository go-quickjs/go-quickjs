// Package wast parses WebAssembly script (.wast) files, the format of the
// specification's test suites, into commands: modules to define and
// instantiate, actions to perform and assertions about their outcomes.
//
// It reads the scripts of WebAssembly 3.0's suites, the threads proposal's
// (thread and wait) and the legacy exception tests. Text modules are
// compiled to binary when the script is parsed; a quoted module's text is
// compiled only when Module.Binary is called, since assert_malformed
// expects that to fail.
package wast

import (
	"fmt"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/wasm/internal/text"
)

// Command is one of the script's commands: *ModuleCommand, *InstanceCommand,
// *RegisterCommand, *ActionCommand, *AssertReturn, *AssertTrap,
// *AssertExhaustion, *AssertException, *AssertInvalid, *AssertMalformed,
// *AssertUnlinkable, *AssertUninstantiable, *ThreadCommand or
// *WaitCommand.
type Command interface {
	// Line is the line of the command's opening parenthesis.
	Line() int
}

// ModuleKind says how a module is given.
type ModuleKind uint8

const (
	// TextModule is a module in the text format, compiled when parsed.
	TextModule ModuleKind = iota
	// BinaryModule is (module binary "..."), its bytes given as strings.
	BinaryModule
	// QuoteModule is (module quote "..."), its text given as strings.
	QuoteModule
)

// Module is a module given in a script.
type Module struct {
	Kind ModuleKind
	// Name is the module's $name, without the '$', or "".
	Name string
	// Text is a quoted module's text.
	Text   []byte
	Line   int
	binary []byte
}

// Binary returns the module's binary: a text module's compiled form, a
// binary module's bytes, or the compiled text of a quoted module, which
// fails if the text is malformed.
func (m *Module) Binary() ([]byte, error) {
	if m.Kind == QuoteModule {
		return text.Compile(m.Text)
	}
	return m.binary, nil
}

// ModuleCommand is (module ...): a module definition, which a script
// command without "definition" also instantiates, under the same name.
type ModuleCommand struct {
	Module *Module
	// Definition is set for (module definition ...), which only defines.
	Definition bool
}

// InstanceCommand is (module instance $instance? $module?): an instance of
// a defined module, the last one if Module is "".
type InstanceCommand struct {
	At       int
	Instance string
	Module   string
}

// RegisterCommand is (register "as" $instance?), which makes an instance's
// exports importable under the module name As.
type RegisterCommand struct {
	At       int
	As       string
	Instance string
}

// ActionKind is an action's kind.
type ActionKind uint8

const (
	// Invoke calls an exported function.
	Invoke ActionKind = iota
	// Get reads an exported global.
	Get
)

// Action is (invoke $instance? "name" arg*) or (get $instance? "name"),
// on the last instance if Instance is "".
type Action struct {
	Kind     ActionKind
	Instance string
	Name     string
	Args     []Value
	At       int
}

// ActionCommand is an action performed for its effect.
type ActionCommand struct{ Action Action }

// AssertReturn asserts that an action returns results matching Results.
type AssertReturn struct {
	At      int
	Action  Action
	Results []Result
}

// AssertTrap asserts that an action traps with a message starting with
// Message.
type AssertTrap struct {
	At      int
	Action  Action
	Message string
}

// AssertExhaustion asserts that an action exhausts the call stack.
type AssertExhaustion struct {
	At      int
	Action  Action
	Message string
}

// AssertException asserts that an action throws a WebAssembly exception.
type AssertException struct {
	At     int
	Action Action
}

// AssertInvalid asserts that a module fails validation. Custom marks
// assert_invalid_custom, about a custom section's contents.
type AssertInvalid struct {
	At      int
	Module  *Module
	Message string
	Custom  bool
}

// AssertMalformed asserts that a module fails to decode or, quoted, to
// parse. Custom marks assert_malformed_custom.
type AssertMalformed struct {
	At      int
	Module  *Module
	Message string
	Custom  bool
}

// AssertUnlinkable asserts that instantiating a module fails to link: the
// module given, or, if Module is nil, the defined module named Definition
// ((module instance ...) form).
type AssertUnlinkable struct {
	At         int
	Module     *Module
	Definition string
	Message    string
}

// AssertUninstantiable asserts that instantiating a module traps, in its
// start function or its segments' initialization: assert_trap of a module.
// Module and Definition are as for AssertUnlinkable.
type AssertUninstantiable struct {
	At         int
	Module     *Module
	Definition string
	Message    string
}

// ThreadCommand is (thread $name (shared (module $m))* command*): commands
// to run on a thread of their own, which may use the named modules, shared
// with the script, and none of its other instances.
type ThreadCommand struct {
	At       int
	Name     string
	Shared   []string
	Commands []Command
}

// WaitCommand is (wait $thread): waits for a thread to finish.
type WaitCommand struct {
	At     int
	Thread string
}

func (c *ModuleCommand) Line() int        { return c.Module.Line }
func (c *InstanceCommand) Line() int      { return c.At }
func (c *RegisterCommand) Line() int      { return c.At }
func (c *ActionCommand) Line() int        { return c.Action.At }
func (c *AssertReturn) Line() int         { return c.At }
func (c *AssertTrap) Line() int           { return c.At }
func (c *AssertExhaustion) Line() int     { return c.At }
func (c *AssertException) Line() int      { return c.At }
func (c *AssertInvalid) Line() int        { return c.At }
func (c *AssertMalformed) Line() int      { return c.At }
func (c *AssertUnlinkable) Line() int     { return c.At }
func (c *AssertUninstantiable) Line() int { return c.At }
func (c *ThreadCommand) Line() int        { return c.At }
func (c *WaitCommand) Line() int          { return c.At }

// Parse parses a script.
func Parse(src []byte) (cmds []Command, err error) {
	toks, annots, err := text.Lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, annots: annots, src: src}
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(*text.Error)
			if !ok {
				panic(r)
			}
			cmds, err = nil, e
		}
	}()
	// A script may be a module's fields alone.
	if t := p.at(1); p.peek().Kind == text.LParen && t.Kind == text.Keyword && moduleFields[t.Text] {
		bin, err := text.Compile(src)
		if err != nil {
			return nil, err
		}
		m := &Module{Kind: TextModule, Line: p.peek().Line, binary: bin}
		return []Command{&ModuleCommand{Module: m}}, nil
	}
	for p.peek().Kind != text.EOF {
		cmds = append(cmds, p.command())
	}
	return cmds, nil
}

var moduleFields = map[string]bool{
	"type": true, "rec": true, "func": true, "import": true, "export": true, "table": true,
	"memory": true, "global": true, "tag": true, "elem": true, "data": true, "start": true,
}

type parser struct {
	toks   []text.Token
	annots []text.Annot
	src    []byte
	p      int
}

func (p *parser) fail(t text.Token, format string, args ...any) {
	panic(&text.Error{Line: t.Line, Col: t.Col, Msg: fmt.Sprintf(format, args...)})
}

func (p *parser) unexpected(t text.Token) {
	if t.Kind == text.EOF {
		p.fail(t, "unexpected end of input")
	}
	p.fail(t, "unexpected token %q", t.Text)
}

func (p *parser) peek() text.Token { return p.toks[p.p] }

func (p *parser) at(k int) text.Token {
	if p.p+k >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.p+k]
}

func (p *parser) next() text.Token {
	t := p.toks[p.p]
	if t.Kind != text.EOF {
		p.p++
	}
	return t
}

func (p *parser) isKw(k int, kw string) bool {
	t := p.at(k)
	return t.Kind == text.Keyword && t.Text == kw
}

func (p *parser) isParen(kw string) bool {
	return p.peek().Kind == text.LParen && p.isKw(1, kw)
}

func (p *parser) lparen() text.Token {
	t := p.next()
	if t.Kind != text.LParen {
		p.unexpected(t)
	}
	return t
}

func (p *parser) rparen() {
	if t := p.next(); t.Kind != text.RParen {
		p.unexpected(t)
	}
}

func (p *parser) optID() string {
	if p.peek().Kind == text.ID {
		return p.next().Text
	}
	return ""
}

func (p *parser) str() string {
	t := p.next()
	if t.Kind != text.String {
		p.unexpected(t)
	}
	return t.Text
}

func (p *parser) name() string {
	t := p.peek()
	s := p.str()
	if !utf8.ValidString(s) {
		p.fail(t, "malformed UTF-8 encoding")
	}
	return s
}

func (p *parser) command() Command {
	open := p.lparen()
	kw := p.next()
	if kw.Kind != text.Keyword {
		p.unexpected(kw)
	}
	line := open.Line
	switch kw.Text {
	case "module":
		p.p -= 2
		return p.moduleCommand()
	case "register":
		c := &RegisterCommand{At: line, As: p.name(), Instance: p.optID()}
		p.rparen()
		return c
	case "invoke", "get":
		p.p -= 2
		return &ActionCommand{Action: p.action()}
	case "assert_return":
		c := &AssertReturn{At: line, Action: p.action()}
		for p.peek().Kind == text.LParen {
			c.Results = append(c.Results, p.result())
		}
		p.rparen()
		return c
	case "assert_trap":
		if p.isParen("module") {
			m, def := p.instanceOrModule()
			c := &AssertUninstantiable{At: line, Module: m, Definition: def, Message: p.str()}
			p.rparen()
			return c
		}
		c := &AssertTrap{At: line, Action: p.action(), Message: p.str()}
		p.rparen()
		return c
	case "assert_uninstantiable":
		m, def := p.instanceOrModule()
		c := &AssertUninstantiable{At: line, Module: m, Definition: def, Message: p.str()}
		p.rparen()
		return c
	case "assert_exhaustion":
		c := &AssertExhaustion{At: line, Action: p.action(), Message: p.str()}
		p.rparen()
		return c
	case "assert_exception":
		c := &AssertException{At: line, Action: p.action()}
		p.rparen()
		return c
	case "assert_invalid", "assert_invalid_custom":
		m, _ := p.module()
		c := &AssertInvalid{At: line, Module: m, Message: p.str(), Custom: kw.Text == "assert_invalid_custom"}
		p.rparen()
		return c
	case "assert_malformed", "assert_malformed_custom":
		m, _ := p.module()
		c := &AssertMalformed{At: line, Module: m, Message: p.str(), Custom: kw.Text == "assert_malformed_custom"}
		p.rparen()
		return c
	case "assert_unlinkable":
		m, def := p.instanceOrModule()
		c := &AssertUnlinkable{At: line, Module: m, Definition: def, Message: p.str()}
		p.rparen()
		return c
	case "thread":
		c := &ThreadCommand{At: line, Name: p.optID()}
		for p.isParen("shared") {
			p.p += 2
			p.lparen()
			if !p.isKw(0, "module") {
				p.unexpected(p.peek())
			}
			p.p++
			t := p.next()
			if t.Kind != text.ID {
				p.unexpected(t)
			}
			c.Shared = append(c.Shared, t.Text)
			p.rparen()
			p.rparen()
		}
		for p.peek().Kind != text.RParen {
			c.Commands = append(c.Commands, p.command())
		}
		p.rparen()
		return c
	case "wait":
		c := &WaitCommand{At: line, Thread: p.optID()}
		p.rparen()
		return c
	}
	p.fail(kw, "unknown command %s", kw.Text)
	return nil
}

// moduleCommand parses a module command: a module, a module definition or
// a module instance.
func (p *parser) moduleCommand() Command {
	if p.isKw(2, "instance") {
		open := p.lparen()
		p.p++
		p.p++
		c := &InstanceCommand{At: open.Line}
		first := p.optID()
		second := p.optID()
		if second != "" {
			c.Instance, c.Module = first, second
		} else {
			c.Module = first
		}
		p.rparen()
		return c
	}
	m, def := p.module()
	return &ModuleCommand{Module: m, Definition: def}
}

// instanceOrModule parses the module of assert_unlinkable or assert_trap:
// a module, or (module instance ...) of a definition.
func (p *parser) instanceOrModule() (*Module, string) {
	if p.isParen("module") && p.isKw(2, "instance") {
		p.p += 3
		first := p.optID()
		second := p.optID()
		p.rparen()
		if second != "" {
			return nil, second
		}
		return nil, first
	}
	m, def := p.module()
	if def {
		p.fail(p.peek(), "misplaced module definition")
	}
	return m, ""
}

// module parses (module definition? $id? ...) in any of its forms.
func (p *parser) module() (*Module, bool) {
	open := p.lparen()
	if !p.isKw(0, "module") {
		p.unexpected(p.peek())
	}
	p.p++
	def := false
	if p.isKw(0, "definition") {
		p.p++
		def = true
	}
	m := &Module{Line: open.Line}
	var nameTok *text.Token
	if p.peek().Kind == text.ID {
		t := p.next()
		nameTok = &t
		m.Name = t.Text
	}
	switch {
	case p.isKw(0, "binary"):
		p.p++
		m.Kind = BinaryModule
		for p.peek().Kind == text.String {
			m.binary = append(m.binary, p.next().Text...)
		}
		if m.binary == nil {
			m.binary = []byte{}
		}
		p.rparen()
	case p.isKw(0, "quote"):
		p.p++
		m.Kind = QuoteModule
		for p.peek().Kind == text.String {
			m.Text = append(m.Text, p.next().Text...)
		}
		p.rparen()
	default:
		m.Kind = TextModule
		end := p.closing(p.p - 1)
		bin, err := text.CompileFields(p.toks, p.annots, p.p, end, nameTok)
		if err != nil {
			panic(err)
		}
		m.binary = bin
		p.p = end + 1
	}
	return m, def
}

// closing returns the index of the parenthesis that closes the one open at
// token from or before it.
func (p *parser) closing(from int) int {
	depth := 1
	for i := from + 1; i < len(p.toks); i++ {
		switch p.toks[i].Kind {
		case text.LParen:
			depth++
		case text.RParen:
			depth--
			if depth == 0 {
				return i
			}
		case text.EOF:
			p.unexpected(p.toks[i])
		}
	}
	p.unexpected(p.toks[len(p.toks)-1])
	return 0
}

func (p *parser) action() Action {
	open := p.lparen()
	kw := p.next()
	a := Action{At: open.Line}
	switch {
	case kw.Kind == text.Keyword && kw.Text == "invoke":
		a.Kind = Invoke
	case kw.Kind == text.Keyword && kw.Text == "get":
		a.Kind = Get
	default:
		p.unexpected(kw)
	}
	a.Instance = p.optID()
	a.Name = p.name()
	if a.Kind == Invoke {
		for p.peek().Kind == text.LParen {
			a.Args = append(a.Args, p.literal())
		}
	}
	p.rparen()
	return a
}
