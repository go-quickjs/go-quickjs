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
	// standard is the script as the standard has it, which Compile made.
	standard *bytecodeFunc
	// quirks is the script as a runtime WithNodeQuirks compiles it, made the
	// first time one runs it.
	quirksOnce sync.Once
	quirks     *bytecodeFunc
	quirksErr  error
}

// Compile parses and compiles a script, to run with RunProgram. The name is
// what its stack traces call it, as EvalFile's is. With strict set the whole
// script is strict code, as though it began with "use strict". Source that
// fails to parse is a *SyntaxError.
func Compile(name, src string, strict bool) (*Program, error) {
	fn, err := compileScript(src, name, 0, 0, strict, false)
	if err != nil {
		return nil, err
	}
	return &Program{name: name, src: src, strict: strict, standard: fn}, nil
}

// code is the program as the runtime compiles source: WithNodeQuirks parses
// and compiles some code as V8 does, which the program is then compiled for
// too, once, however many runtimes run it.
func (p *Program) code(r *Runtime) (*bytecodeFunc, error) {
	if !r.nodeQuirks {
		return p.standard, nil
	}
	p.quirksOnce.Do(func() {
		p.quirks, p.quirksErr = compileScript(p.src, p.name, 0, 0, p.strict, true)
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
