package quickjs

import (
	"context"
	"errors"
	"sync"
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
	// standard is the script as the standard has it, made by Compile, or
	// the first time a runtime without node quirks runs one that a
	// runtime's Compile made.
	standardOnce sync.Once
	standard     *bytecodeFunc
	standardErr  error
	// quirks is the script as a runtime WithNodeQuirks compiles it, made the
	// first time one runs it.
	quirksOnce sync.Once
	quirks     *bytecodeFunc
	quirksErr  error
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
	fn, err := compileScript(src, name, c.line, c.column, c.strict, false)
	if err != nil {
		return nil, err
	}
	return &Program{name: name, src: src, strict: c.strict, line: c.line, column: c.column, standard: fn}, nil
}

// Compile is the package's Compile as this runtime parses source, which is
// as V8 does where it departs from the standard if the runtime was made
// WithNodeQuirks: what Eval would accept, a program from here accepts. The
// program may still be run by any runtime.
func (r *Runtime) Compile(name, src string, opts ...CompileOption) (*Program, error) {
	if !r.nodeQuirks {
		return Compile(name, src, opts...)
	}
	var c compileConfig
	for _, o := range opts {
		o(&c)
	}
	fn, err := compileScript(src, name, c.line, c.column, c.strict, true)
	if err != nil {
		return nil, err
	}
	p := &Program{name: name, src: src, strict: c.strict, line: c.line, column: c.column}
	p.quirksOnce.Do(func() { p.quirks = fn })
	return p, nil
}

// CodeGenerationAllowed reports whether the runtime compiles source a script
// hands it, which a runtime made WithoutCodeGeneration does not: a host that
// compiles such source for the script -- node:vm's Script -- refuses it then,
// as eval is refused.
func (r *Runtime) CodeGenerationAllowed() bool { return !r.noCodeGeneration }

// code is the program as the runtime compiles source: WithNodeQuirks parses
// and compiles some code as V8 does, which the program is then compiled for
// too, once, however many runtimes run it.
func (p *Program) code(r *Runtime) (*bytecodeFunc, error) {
	if !r.nodeQuirks {
		p.standardOnce.Do(func() {
			if p.standard == nil {
				p.standard, p.standardErr = compileScript(p.src, p.name, p.line, p.column, p.strict, false)
			}
		})
		return p.standard, p.standardErr
	}
	p.quirksOnce.Do(func() {
		p.quirks, p.quirksErr = compileScript(p.src, p.name, p.line, p.column, p.strict, true)
	})
	return p.quirks, p.quirksErr
}

// RunProgram runs a compiled program, as Eval runs source, and returns its
// completion value.
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
	return r.runIn(ctx, nil, fn)
}
