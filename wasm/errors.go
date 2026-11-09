package wasm

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/internal/engine"
)

// CompileError is a module that does not decode or validate: the first
// error found, and the offset in the module's bytes it was found at.
type CompileError struct {
	// Msg describes the error, e.g. "Compiling function #0 failed: i32.add[1]
	// expected type i32, found i64.const of type i64".
	Msg string
	// Offset is where in the module's bytes the error was found.
	Offset uint32
}

// Error returns "<message> @+<offset>".
func (e *CompileError) Error() string {
	return e.Msg + " @+" + strconv.FormatUint(uint64(e.Offset), 10)
}

func compileError(e *decoder.Error) *CompileError {
	return &CompileError{Msg: e.Msg, Offset: e.Offset}
}

// LinkError is an import that could not be resolved: a module or a name the
// runtime does not have, or an entity that does not match the import's
// declaration.
type LinkError struct {
	// Msg names the import and what is wrong, e.g. `Import #0 "env" "f":
	// imported function does not match the expected type`.
	Msg string
}

// Error returns the message.
func (e *LinkError) Error() string { return e.Msg }

// RuntimeError is a trap: an instruction that could not run, such as an
// integer division by zero, or the call stack's exhaustion.
type RuntimeError struct {
	msg    string
	frames []StackFrame
}

// StackTraceLimit is how many frames RuntimeError.Stack prints.
const StackTraceLimit = 10

// StackFrame is a wasm function on a RuntimeError's call stack.
type StackFrame struct {
	// URL identifies the module: wasm://wasm/ followed by the name in the
	// module's name section and a dash, if it has one, and eight hex
	// digits of a hash of its bytes.
	URL string
	// Name is the function as the stack trace names it: the module's name
	// and the function's, joined by a dot, either of them alone, or empty
	// when the module's name section names neither.
	Name string
	// FunctionIndex is the function's index in its module.
	FunctionIndex uint32
	// Offset is the module offset of the instruction executing: for a
	// frame that made a call, the call. For the frame that trapped it is
	// the offset of the last call that frame made, or zero, until the
	// interpreter records where each trap happens.
	Offset uint32
}

// Error returns the trap's message: "unreachable", "divide by zero",
// "memory access out of bounds", "Maximum call stack size exceeded", ...
func (e *RuntimeError) Error() string { return e.msg }

// Frames returns the wasm call stack where the trap happened, innermost
// first. Host functions are left out.
func (e *RuntimeError) Frames() []StackFrame { return e.frames }

// Stack returns the call stack, innermost first, one line for each of the
// innermost StackTraceLimit frames, each indented by four spaces:
//
//	at name (wasm://wasm/1a2b3c4d:wasm-function[1]:0x3a)
//
// or, for a function without a name,
//
//	at wasm://wasm/1a2b3c4d:wasm-function[1]:0x3a
//
// Frames returns every frame.
func (e *RuntimeError) Stack() string {
	var b strings.Builder
	for i, f := range e.frames {
		if i == StackTraceLimit {
			break
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(f.String())
	}
	return b.String()
}

// String returns the frame's line of a stack trace.
func (f StackFrame) String() string {
	loc := fmt.Sprintf("%s:wasm-function[%d]:0x%x", f.URL, f.FunctionIndex, f.Offset)
	if f.Name == "" {
		return "    at " + loc
	}
	return "    at " + f.Name + " (" + loc + ")"
}

// Exception is a wasm exception that no wasm code caught.
type Exception struct {
	// Values are the exception's payload, as its tag's type gives them.
	Values []api.Value
}

// Error returns "uncaught wasm exception".
func (e *Exception) Error() string { return "uncaught wasm exception" }

// runtimeError converts a trap.
func (r *runtime) runtimeError(t *engine.Trap) *RuntimeError {
	e := &RuntimeError{msg: t.Error()}
	for _, f := range t.Frames {
		if f.Module == nil {
			continue
		}
		info := r.codeOf(f.Module).names()
		sf := StackFrame{URL: info.url, FunctionIndex: f.Func, Offset: f.Offset}
		if r.cfg.debugInfo {
			sf.Name = info.frameName(f.Func)
		}
		e.frames = append(e.frames, sf)
	}
	return e
}

// moduleURL returns the URL of a module of bytes b, named name if hasName.
func moduleURL(b []byte, name string, hasName bool) string {
	h := fmt.Sprintf("%08x", wireBytesHash(b))
	if hasName {
		return "wasm://wasm/" + name + "-" + h
	}
	return "wasm://wasm/" + h
}

// wireBytesHash is the hash a module's bytes are named by in stack traces:
// the rapidhash of the bytes, with seed zero and the default secret, cut to
// 30 bits, as a string hash field. A module over 16,383 bytes is named by its
// length.
func wireBytesHash(b []byte) uint32 {
	const maxHashCalcLength = 16383
	var h uint32
	if len(b) > maxHashCalcLength {
		h = uint32(len(b))
	} else {
		h = uint32(rapidhash(b, 0)) & (1<<30 - 1)
		if h == 0 {
			h = 27
		}
	}
	return h<<2 | 2
}

var rapidSecret = [3]uint64{0x2d358dccaa6c78a5, 0x8bb84b93962eacc9, 0x4b33a62ed433d4a3}

func rapidMix(a, b uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return hi ^ lo
}

// rapidhash hashes p: rapidhash by Wang Yi, public domain.
func rapidhash(p []byte, seed uint64) uint64 {
	r64 := func(p []byte) uint64 { return binary.LittleEndian.Uint64(p) }
	r32 := func(p []byte) uint64 { return uint64(binary.LittleEndian.Uint32(p)) }
	n := uint64(len(p))
	s := &rapidSecret
	seed ^= rapidMix(seed^s[0], s[1]) ^ n
	var a, b uint64
	switch {
	case n >= 4 && n <= 16:
		last := n - 4
		a = r32(p)<<32 | r32(p[last:])
		d := (n & 24) >> (n >> 3)
		b = r32(p[d:])<<32 | r32(p[last-d:])
	case n > 0 && n < 4:
		a = uint64(p[0])<<56 | uint64(p[n>>1])<<32 | uint64(p[n-1])
	case n > 16:
		q, i := p, n
		if i > 48 {
			s1, s2 := seed, seed
			for i >= 48 {
				seed = rapidMix(r64(q)^s[0], r64(q[8:])^seed)
				s1 = rapidMix(r64(q[16:])^s[1], r64(q[24:])^s1)
				s2 = rapidMix(r64(q[32:])^s[2], r64(q[40:])^s2)
				q, i = q[48:], i-48
			}
			seed ^= s1 ^ s2
		}
		if i > 16 {
			seed = rapidMix(r64(q)^s[2], r64(q[8:])^seed^s[1])
			if i > 32 {
				seed = rapidMix(r64(q[16:])^s[2], r64(q[24:])^seed)
			}
		}
		a, b = r64(p[n-16:]), r64(p[n-8:])
	}
	a ^= s[1]
	b ^= seed
	hi, lo := bits.Mul64(a, b)
	return rapidMix(lo^s[0]^n, hi^s[1])
}
