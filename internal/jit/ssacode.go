//go:build quickjs_jit && !android && !ios && (((linux || windows) && amd64) || (darwin && arm64))

package jit

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// The backend for this target is compiled in.
const supported = true

// SSACode owns a function the new pipeline compiled (internal/jit/mir) and
// its executable memory. It belongs to one runtime.
type SSACode struct {
	code  []byte
	arena *Arena
	// entries is each slot IR PC's code offset, or -1: a slice, since every
	// exit to Go looks one up.
	entries []int32
	// exits are the descriptions the code's exits name by address.
	exits []*abi.ExitDescriptor
	// footprint is what the code holds (Footprint), which never changes
	// while it is open.
	footprint int
}

// NewSSACode publishes compiled code in a, a runtime's arena (nil gives it
// one of its own): written, then sealed executable.
func NewSSACode(a *Arena, c *mir.Code) (*SSACode, error) {
	if len(c.Bytes) > MaxCodeBytes {
		// Too much for one placement: the function's, which others' code
		// placed after it does not share. Wrapped as the host's failure
		// to place code, it had the runtime place no more: TypeScript's
		// compiler, one of its functions 1.1 MB of code, ran the rest in
		// the existing tiers.
		return nil, fmt.Errorf("%w: %d bytes", ErrCodeSize, len(c.Bytes))
	}
	if a == nil {
		a = NewArena()
	}
	code, err := a.place(c.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	n := 0
	for pc := range c.Entries {
		n = max(n, pc+1)
	}
	entries := make([]int32, n)
	for i := range entries {
		entries[i] = -1
	}
	for pc, off := range c.Entries {
		entries[pc] = int32(off)
	}
	s := &SSACode{code: code, arena: a, entries: entries, exits: c.Exits}
	s.footprint = footprint(len(code), cap(entries), c.Exits)
	runtime.SetFinalizer(s, func(s *SSACode) { _ = s.Close() })
	return s, nil
}

// HasEntry reports whether the code can be entered at a slot IR PC.
func (s *SSACode) HasEntry(pc int) bool {
	return pc >= 0 && pc < len(s.entries) && s.entries[pc] >= 0
}

// EntryAddress is the address of the code's entry at a slot IR PC, which a
// caller's native code jumps to (mir's native calls), or 0 for none.
func (s *SSACode) EntryAddress(pc int) uintptr {
	if s == nil || len(s.code) == 0 || !s.HasEntry(pc) {
		return 0
	}
	return uintptr(unsafe.Pointer(&s.code[s.entries[pc]]))
}

// Run enters at a slot IR PC with ctx, which must hold the frame's addresses
// and the back-edge counter; it returns when native code exits, with the
// exit record in ctx -- or, for an exit that left its frame to Go
// (abi.ExitTable), in the context that left, the caller's to apply
// (ApplyExit): ctx, or a native callee's, which only the caller knows.
func (s *SSACode) Run(pc int, ctx *abi.Context) error {
	if s == nil || len(s.code) == 0 {
		return ErrClosed
	}
	if !s.HasEntry(pc) {
		return fmt.Errorf("jit: no entry at pc %d", pc)
	}
	enterSSA(&s.code[s.entries[pc]], ctx)
	runtime.KeepAlive(s)
	runtime.KeepAlive(ctx)
	return nil
}

// Resume goes on in s's code at addr, where a native call returns to after
// its callee (abi.Context.ReturnTo), with ctx, the callee's context, which
// holds the call's result (RetValue) and where its frame began (Base): a
// caller's code goes on after a callee Go finished. It returns as Run does.
func (s *SSACode) Resume(addr uintptr, ctx *abi.Context) error {
	if s == nil || len(s.code) == 0 {
		return ErrClosed
	}
	base := uintptr(unsafe.Pointer(&s.code[0]))
	if addr < base || addr >= base+uintptr(len(s.code)) {
		return fmt.Errorf("jit: %#x is not in the code", addr)
	}
	enterSSA(&s.code[addr-base], ctx)
	runtime.KeepAlive(s)
	runtime.KeepAlive(ctx)
	return nil
}

// Contains reports whether addr is in s's code, which is open: where a
// native call in it returns to (abi.Context.ReturnTo) is, and in no other
// code, though the function was compiled again since.
func (s *SSACode) Contains(addr uintptr) bool {
	if s == nil || len(s.code) == 0 {
		return false
	}
	base := uintptr(unsafe.Pointer(&s.code[0]))
	return addr >= base && addr < base+uintptr(len(s.code))
}

// Footprint is the memory s holds, which its owner counts against its
// budget: the executable memory it owns, as placed (CodeFootprint), and
// what describes it -- its entries and its exits' descriptions.
func (s *SSACode) Footprint() int {
	if s == nil || len(s.code) == 0 {
		return 0
	}
	return s.footprint
}

// CodeFootprint is what c's code will hold once placed (SSACode's
// Footprint): what a runtime asks before it places the code, to make room
// for it or refuse it.
func CodeFootprint(c *mir.Code) int {
	n := 0
	for pc := range c.Entries {
		n = max(n, pc+1)
	}
	return footprint(alignUp(len(c.Bytes), codeAlign), n, c.Exits)
}

func footprint(code, entries int, exits []*abi.ExitDescriptor) int {
	n := code + int(unsafe.Sizeof(SSACode{})) + entries*4 + cap(exits)*8
	var frame *abi.ExitFrame
	for _, x := range exits {
		n += int(unsafe.Sizeof(*x)) + cap(x.Inline)*int(unsafe.Sizeof(abi.ExitInline{}))
		if f := x.Frame; f != frame {
			// One for the code's descriptions (mir's exitDescriptor).
			frame = f
			n += int(unsafe.Sizeof(*f)) + cap(f.Slots)*int(unsafe.Sizeof(abi.ExitSlot{})) + cap(f.Index)*2 + cap(f.Words)*8
		}
	}
	return n
}

// Size is the executable memory s owns.
func (s *SSACode) Size() int {
	if s == nil {
		return 0
	}
	return len(s.code)
}

// Close releases the executable memory; it is idempotent.
func (s *SSACode) Close() error {
	if s == nil || len(s.code) == 0 {
		return nil
	}
	if err := s.arena.release(s.code); err != nil {
		return err
	}
	s.code, s.arena = nil, nil
	runtime.SetFinalizer(s, nil)
	return nil
}

//go:noescape
func enterSSA(code *byte, ctx *abi.Context)

// callGo is where native code calls Go (entry_*.s), and callGoAddr its
// address.
func callGo()
func callGoAddr() uintptr

// CallGo is where native code jumps to call Go (abi.Encoding's CallGo).
func CallGo() uint64 { return uint64(callGoAddr()) }

// HostCall is Go's side of a call from native code (abi.Context's GoOp):
// it does what ctx.GoOp says with ctx.GoArgs, its result to
// ctx.Keep[ctx.GoKeep], and reports whether it did. It is set once, by the
// VM, before any code runs.
var HostCall func(ctx *abi.Context) bool

// goCall is what callGo calls: HostCall, whose answer it puts in
// ctx.GoStatus; it returns where the code goes on.
func goCall(ctx *abi.Context) uintptr {
	ctx.GoStatus = 1
	if HostCall != nil && HostCall(ctx) {
		ctx.GoStatus = 0
	}
	return ctx.GoResume
}
