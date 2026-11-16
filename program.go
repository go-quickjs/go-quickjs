package quickjs

import (
	"context"
	"errors"
	"sync"

	"github.com/go-quickjs/go-quickjs/internal/vm"
)

// Program is a script compiled once, which any number of runtimes may then
// run with RunProgram, as goja's Program is: the source is parsed and
// compiled once rather than by every runtime that runs it. A Program holds no
// runtime's values, and is safe to share between goroutines, and to run on
// several runtimes at once.
//
// A program is compiled as the standard has it. A runtime WithNodeQuirks runs
// it as that runtime would compile it, so source only V8 accepts -- strict code
// assigning to a call -- does not compile, and is run with EvalFile instead.
type Program struct {
	name, src string
	strict    bool
	// line and column place the source within the file it is named for.
	line, column int
	// forms is the script as each kind of runtime compiles it, by
	// programForm: as the standard has it, made by Compile, or as a
	// runtime WithNodeQuirks compiles it, each with its register form or
	// without, for a runtime that runs no register code (one with the
	// JIT). Each is made the first time a runtime of its kind runs the
	// program, if Compile did not make it.
	forms [4]programCode
}

// programCode is one of a Program's forms.
type programCode struct {
	once sync.Once
	fn   *bytecodeFunc
	err  error
}

// programForm is the index in Program.forms of the form compiled with node
// quirks or not, and with no register form or with one.
func programForm(quirks, noReg bool) int {
	i := 0
	if quirks {
		i += 2
	}
	if noReg {
		i++
	}
	return i
}

// CompileOption configures Compile, as an Option configures New.
type CompileOption func(*compileConfig)

type compileConfig struct {
	strict       bool
	line, column int
}

// WithOffset places the source within a larger file -- a script in an HTML
// page, a function body a host wraps -- so that stack traces and syntax errors
// give positions in the whole file: its first line is line lines down, and
// that line starts column columns in. Later lines start where they do.
func WithOffset(line, column int) CompileOption {
	return func(c *compileConfig) { c.line, c.column = line, column }
}

// WithStrict compiles the whole script as strict code, as though it began
// with "use strict".
func WithStrict() CompileOption {
	return func(c *compileConfig) { c.strict = true }
}

// Compile parses and compiles a script, to run with RunProgram. The name is
// what its stack traces call it, as EvalFile's is. Source that fails to parse
// is a *SyntaxError.
func Compile(name, src string, opts ...CompileOption) (*Program, error) {
	var c compileConfig
	for _, o := range opts {
		o(&c)
	}
	// The form a runtime without the JIT runs, where the process runs
	// register code (vm.RegTierOn); any other is made when it is needed.
	noReg := !vm.RegTierOn()
	fn, err := compileScript(src, name, c.line, c.column, c.strict, false, false, noReg)
	if err != nil {
		return nil, err
	}
	p := &Program{name: name, src: src, strict: c.strict, line: c.line, column: c.column}
	p.forms[programForm(false, noReg)].set(fn)
	return p, nil
}

// set makes c fn, compiled already.
func (c *programCode) set(fn *bytecodeFunc) { c.once.Do(func() { c.fn = fn }) }

// Compile is the package's Compile as this runtime parses source, which is
// as V8 does where it departs from the standard if the runtime was made
// WithNodeQuirks: what Eval would accept, a program from here accepts. The
// program may still be run by any runtime.
func (r *Runtime) Compile(name, src string, opts ...CompileOption) (*Program, error) {
	noReg := !r.rt.RegTier()
	if !r.nodeQuirks && noReg == !vm.RegTierOn() {
		return Compile(name, src, opts...)
	}
	var c compileConfig
	for _, o := range opts {
		o(&c)
	}
	fn, err := compileScript(src, name, c.line, c.column, c.strict, r.nodeQuirks, false, noReg)
	if err != nil {
		return nil, err
	}
	p := &Program{name: name, src: src, strict: c.strict, line: c.line, column: c.column}
	p.forms[programForm(r.nodeQuirks, noReg)].set(fn)
	return p, nil
}

// CodeGenerationAllowed reports whether the runtime compiles source a script
// hands it, which a runtime made WithoutCodeGeneration does not: a host that
// compiles such source for the script -- node:vm's Script -- refuses it then,
// as eval is refused.
func (r *Runtime) CodeGenerationAllowed() bool { return !r.noCodeGeneration }

// code is the program as the runtime compiles source: WithNodeQuirks parses
// and compiles some code as V8 does, and a runtime with the JIT runs no
// register code, which the program is then compiled for too, once, however
// many runtimes run it. Runtimes of different kinds never share code: the
// tree a function runs as is the same in every runtime that runs it.
func (p *Program) code(r *Runtime) (*bytecodeFunc, error) {
	noReg := !r.rt.RegTier()
	if r.debug {
		// Code compiled for a debugger is the runtime's own, so the
		// program is compiled for it each time it runs.
		return compileScript(p.src, p.name, p.line, p.column, p.strict, r.nodeQuirks, true, noReg)
	}
	c := &p.forms[programForm(r.nodeQuirks, noReg)]
	c.once.Do(func() {
		c.fn, c.err = compileScript(p.src, p.name, p.line, p.column, p.strict, r.nodeQuirks, false, noReg)
	})
	return c.fn, c.err
}

// RunProgram runs a compiled program, as Eval runs source, and returns its
// completion value. A program run again in a realm finds what its last run
// learned of its object literals' layouts, and makes none of them anew, where
// Eval of the same source, which compiles it again, does.
func (r *Runtime) RunProgram(p *Program) (Value, error) {
	return r.RunProgramContext(context.Background(), p)
}

// RunProgramContext is RunProgram with cancellation, as EvalContext is Eval
// with it.
func (r *Runtime) RunProgramContext(ctx context.Context, p *Program) (result Value, err error) {
	if r.closed {
		return Value{}, ErrClosed
	}
	if p == nil {
		return Value{}, errors.New("quickjs: RunProgram of a nil *Program")
	}
	defer r.guard(&err)
	fn, err := p.code(r)
	if err != nil {
		return Value{}, err
	}
	return r.runCode(ctx, nil, fn, !r.debug)
}
