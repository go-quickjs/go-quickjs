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
	// callEntry and resume are the offsets of the code's call entry and
	// resume routine, or -1 (mir's Code).
	callEntry, resume int32
	// exits are the descriptions the code's exits name by address, calls
	// those its native calls do (abi.Context's CallState); callRefs bounds
	// the pointer words its calls write themselves (mir's Code).
	exits    []*abi.ExitDescriptor
	calls    []*abi.CallState
	goCalls  []*abi.GoCallState
	callRefs int
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
	s := &SSACode{code: code, arena: a, entries: entries, callEntry: int32(c.CallEntry), resume: int32(c.Resume), exits: c.Exits,
		calls: c.Calls, goCalls: c.GoCalls, callRefs: c.CallRefs}
	s.footprint = footprint(len(code), cap(entries), c)
	runtime.SetFinalizer(s, func(s *SSACode) { _ = s.Close() })
	return s, nil
}

// GoCallStates is the descriptions of the code's calls of Go
// (abi.Context's GoState), which the code holds: what the VM's leaf keeps
// its account of a call in (abi.GoCallState's BigScore), which a test
// reads.
func (s *SSACode) GoCallStates() []*abi.GoCallState { return s.goCalls }

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

// CallRefs bounds the pointer words the code's native calls write
// themselves to their context's RecordRef (abi.CallRecord's Ref), which Go
// clears as it ends a run.
func (s *SSACode) CallRefs() int {
	if s == nil {
		return 0
	}
	return s.callRefs
}

// CallEntry is the address of the code's call entry, where its native
// callers jump (abi.Context's ReturnTo), or 0 for none.
func (s *SSACode) CallEntry() uintptr {
	if s == nil || len(s.code) == 0 || s.callEntry < 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&s.code[s.callEntry]))
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
// caller's code goes on after a callee Go finished. Its resume routine
// (mir's Code.Resume) takes the result from ctx as a native callee's return
// leaves it, and goes on at addr, which it finds in ctx's ReturnTo. It
// returns as Run does.
func (s *SSACode) Resume(addr uintptr, ctx *abi.Context) error {
	if s == nil || len(s.code) == 0 {
		return ErrClosed
	}
	base := uintptr(unsafe.Pointer(&s.code[0]))
	if addr < base || addr >= base+uintptr(len(s.code)) || s.resume < 0 {
		return fmt.Errorf("jit: %#x is not in the code", addr)
	}
	ctx.ReturnTo = addr
	enterSSA(&s.code[s.resume], ctx)
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
	return footprint(alignUp(len(c.Bytes), codeAlign), n, c)
}

func footprint(code, entries int, c *mir.Code) int {
	exits, calls := c.Exits, c.Calls
	n := code + int(unsafe.Sizeof(SSACode{})) + entries*4 + cap(exits)*8 + cap(calls)*8 + cap(c.GoCalls)*8
	var frame *abi.ExitFrame
	frameOf := func(f *abi.ExitFrame) {
		if f != frame {
			// One for the code's descriptions (mir's exitDescriptor).
			frame = f
			n += int(unsafe.Sizeof(*f)) + cap(f.Slots)*int(unsafe.Sizeof(abi.ExitSlot{})) + cap(f.Index)*2 + cap(f.Words)*8
		}
	}
	for _, x := range exits {
		n += int(unsafe.Sizeof(*x)) + cap(x.Inline)*int(unsafe.Sizeof(abi.ExitInline{}))
		frameOf(x.Frame)
	}
	for _, st := range calls {
		n += int(unsafe.Sizeof(*st)) + cap(st.Records)*int(unsafe.Sizeof(abi.CallRecord{}))
		frameOf(st.Frame)
	}
	for _, st := range c.GoCalls {
		n += int(unsafe.Sizeof(*st)) + cap(st.Args)*int(unsafe.Sizeof(abi.GoCallArg{}))
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

// storeRef is where native code stores a pointer word while the collector
// marks (entry_*.s), and storeRefAddr its address: V8's code's call of its
// write barrier, a routine that saves nothing native code keeps, calling
// storeRefGo.
func storeRef()
func storeRefAddr() uintptr

// StoreRef is where native code jumps to store a pointer word with Go's
// write barrier (abi.Encoding's StoreRef).
func StoreRef() uint64 { return uint64(storeRefAddr()) }

// storeRefClobber is storeRef, every register native code may keep given
// junk first, and storeRefClobberAddr its address.
func storeRefClobber()
func storeRefClobberAddr() uintptr

// StoreRefClobber is StoreRef for tests: what native code jumps to there
// fills every register it may keep with junk before it stores, so that code
// that did not save one it keeps across the store goes wrong.
func StoreRefClobber() uint64 { return uint64(storeRefClobberAddr()) }

// storeRefGo is what storeRef calls: the store, which compiled Go makes
// with its write barrier while the collector marks -- the old pointer and
// the new shaded, as the hybrid barrier does. It never grows the stack,
// which storeRef's caller, native code, did not check, nor preempts: what
// it does is the barrier's, which compiled Go makes in any function.
// Neither the race detector nor checkptr is told of the store: slot is
// any cell's pointer word, in an object, a frame's slots or the context.
//
//go:nosplit
//go:norace
//go:nocheckptr
func storeRefGo(slot *unsafe.Pointer, p unsafe.Pointer) { *slot = p }

// HostCall is Go's side of a call from native code (abi.Context's GoOp):
// it does what ctx.GoOp says with ctx.GoArgs, its result to
// ctx.Keep[ctx.GoKeep], and reports whether it did. It is set once, by the
// VM, before any code runs.
var HostCall func(ctx *abi.Context) bool

// goCall is what callGo calls: HostCall, whose answer it puts in
// ctx.GoStatus; it returns where the code goes on.
func goCall(ctx *abi.Context) uintptr {
	fail := ExpandGoCall(ctx)
	ctx.GoStatus = 1
	if HostCall != nil && HostCall(ctx) {
		ctx.GoStatus = 0
	} else if fail != 0 {
		// Where the call goes if Go did nothing, its code testing nothing.
		ctx.GoSite += uintptr(int64(fail))
	}
	return ctx.GoResume
}
