//go:build quickjs_jit && amd64 && (linux || windows) && !android && !ios

package engine

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"unsafe"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

const jitBuilt = true

func jitAvailable() bool { return ijit.Supported() }

// codeJIT is a Code's native code: its functions' code in an arena of its
// own, and the link table native calls go through.
type codeJIT struct {
	threshold int32
	// mu serializes compiles.
	mu sync.Mutex
	// run is held for reading while native code of the Code runs, and for
	// writing while code is placed: placing makes pages of the arena
	// writable, which no thread may be running then.
	run   sync.RWMutex
	arena *ijit.Arena
	// links holds each function's native entry, or 0; native code reads
	// it, so it is written atomically and never moves.
	links []uintptr
	raws  []*ijit.RawCode
}

// nativeFn is a function's native code.
type nativeFn struct {
	raw     *ijit.RawCode
	base    uintptr
	goEntry int
	retStub uintptr
	nslots  int
}

// jitInstance is what an instance's native code reads of it: each global's
// address.
type jitInstance struct {
	globals atomic.Pointer[[]uintptr]
}

// jitLevel is one entry of native code from Go, and the native calls it
// makes until it returns: its context, and the state of the exec it
// restores after each exit's work.
type jitLevel struct {
	ctx  wjit.Ctx
	inst *Instance
	cj   *codeJIT
	// rsBase is the address of the level's first return stack entry, the
	// one for the frame Go entered; rsDepth is how many bytes of entries
	// past it the depth limit allows.
	rsBase, rsDepth uintptr
	// nc and chunkBase are the stack chunk the frames are in.
	nc        int
	chunkBase uintptr
	// depth, calls and nsp are the exec's when the level was entered.
	depth, calls, nsp int
	args, res         []Val
}

// jitExec is an exec's native state: the levels entered, and the native
// return stack they share.
type jitExec struct {
	levels []*jitLevel
	n      int
	rs     []uint64
}

func (x *exec) jitLevel() int { return x.jit.n }

func (x *exec) setJITLevel(n int) { x.jit.n = n }

// jitEnv is WASM_JIT's threshold, or -1 for none.
var jitEnv = func() int {
	switch v := os.Getenv("WASM_JIT"); v {
	case "", "off", "0":
		return -1
	case "eager":
		return 0
	case "on", "1":
		return DefaultJITThreshold
	default:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		return -1
	}
}()

func (c *Code) jitFromEnv() {
	if jitEnv >= 0 {
		c.enableJIT(jitEnv)
	}
}

func (c *Code) enableJIT(threshold int) bool {
	if !ijit.Supported() {
		return false
	}
	if threshold < 0 {
		threshold = DefaultJITThreshold
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cj := c.jit.Load(); cj != nil {
		cj.threshold = int32(threshold)
		return true
	}
	cj := &codeJIT{threshold: int32(threshold), arena: ijit.NewArena(), links: make([]uintptr, max(len(c.Module.Functions), 1))}
	runtime.AddCleanup(c, func(cj *codeJIT) { cj.close() }, cj)
	c.jit.Store(cj)
	return true
}

// close gives the code back, once the Code is unreachable.
func (cj *codeJIT) close() {
	cj.mu.Lock()
	defer cj.mu.Unlock()
	for _, r := range cj.raws {
		r.Close()
	}
	cj.raws = nil
}

// native returns function idx's native code, compiling it once it has been
// called often enough, or nil.
func (cj *codeJIT) native(c *Code, idx uint32, cd *compiled) *nativeFn {
	if nat := cd.native.Load(); nat != nil {
		return nat
	}
	if cd.refused.Load() || cd.hot.Add(1) <= cj.threshold {
		return nil
	}
	return cj.compile(c, idx, cd)
}

func (cj *codeJIT) compile(c *Code, idx uint32, cd *compiled) (nat *nativeFn) {
	cj.mu.Lock()
	defer cj.mu.Unlock()
	if nat := cd.native.Load(); nat != nil || cd.refused.Load() {
		return nat
	}
	code, err := compileNative(c, idx, cd)
	if err != nil {
		cd.refused.Store(true)
		reason := "internal"
		if r, ok := err.(*wjit.Refusal); ok {
			reason = r.Reason
		}
		countRefused(reason)
		return nil
	}
	cj.run.Lock()
	raw, err := ijit.PlaceRaw(cj.arena, code.Bytes)
	cj.run.Unlock()
	if err != nil {
		cd.refused.Store(true)
		countRefused("unplaced")
		return nil
	}
	cj.raws = append(cj.raws, raw)
	nat = &nativeFn{raw: raw, base: raw.Base(), goEntry: code.GoEntry, retStub: raw.Base() + uintptr(code.ReturnStub), nslots: cd.nslots}
	cd.native.Store(nat)
	atomic.StoreUintptr(&cj.links[idx], nat.base+uintptr(code.Entry))
	countCompiled()
	return nat
}

// compileNative compiles function idx; a panic of the compiler's is a
// refusal, and the function stays in the interpreter.
func compileNative(c *Code, idx uint32, cd *compiled) (code *wjit.Code, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("wasm jit: compiling function %d: %v", idx, p)
		}
	}()
	return wjit.Compile(&wjit.Request{
		Module:       c.Module,
		Func:         idx,
		FrameSlots:   cd.nslots,
		FrameSlotsOf: func(f uint32) int { return c.get(f).nslots },
		MaxCodeBytes: wjit.MaxCodeBytes,
	})
}

// runNative runs function fn in frame l natively, if it has native code or
// is now compiled, returning the signal its body would: 0, or sigTail with
// the exec's tail call set.
func (x *exec) runNative(fn *Function, code *compiled, l []uint64) (int32, bool) {
	cj := fn.Inst.Code.jit.Load()
	if cj == nil {
		return 0, false
	}
	nat := cj.native(fn.Inst.Code, fn.Index, code)
	if nat == nil {
		return 0, false
	}
	return x.enterNative(cj, fn, nat, l), true
}

// globalsAddr returns the address of the table of the instance's globals'
// addresses, which native code reads them through.
func (inst *Instance) globalsAddr() uintptr {
	if len(inst.Globals) == 0 {
		return 0
	}
	p := inst.jit.globals.Load()
	if p == nil {
		g := make([]uintptr, len(inst.Globals))
		for i, gl := range inst.Globals {
			g[i] = uintptr(unsafe.Pointer(&gl.Num))
		}
		inst.jit.globals.CompareAndSwap(nil, &g)
		p = inst.jit.globals.Load()
	}
	return uintptr(unsafe.Pointer(&(*p)[0]))
}

const rsEntryBytes = wjit.RSEntryWords * 8

// rsStart is the address of the return stack's first word.
func (x *exec) rsStart() uintptr { return uintptr(unsafe.Pointer(&x.jit.rs[0])) }

// rsIndex is the index in the return stack of the word at addr.
func (x *exec) rsIndex(addr uintptr) int { return int((addr - x.rsStart()) / 8) }

// jitRoom makes room for n more entries past the active level's, growing
// the return stack -- and moving every level's pointers into it -- if it
// has none.
func (x *exec) jitRoom(from uintptr, n int) uintptr {
	if x.jit.rs == nil {
		x.jit.rs = make([]uint64, 1024*wjit.RSEntryWords)
		return x.rsStart()
	}
	end := x.rsStart() + uintptr(len(x.jit.rs))*8
	if end-from >= uintptr(n*rsEntryBytes) {
		return from
	}
	old := x.rsStart()
	rs := make([]uint64, 2*len(x.jit.rs)+n*wjit.RSEntryWords)
	copy(rs, x.jit.rs)
	x.jit.rs = rs
	start := x.rsStart()
	newEnd := start + uintptr(len(rs))*8
	for _, lv := range x.jit.levels[:x.jit.n] {
		lv.rsBase = lv.rsBase - old + start
		lv.ctx.RSP = lv.ctx.RSP - old + start
		lv.ctx.RSEnd = min(newEnd, lv.rsBase+lv.rsDepth)
	}
	return from - old + start
}

// enterNative runs fn's native code in frame l, as a new level, and handles
// its exits until the frame returns.
func (x *exec) enterNative(cj *codeJIT, fn *Function, nat *nativeFn, l []uint64) int32 {
	if x.jit.n == len(x.jit.levels) {
		x.jit.levels = append(x.jit.levels, new(jitLevel))
	}
	var from uintptr
	if x.jit.n > 0 {
		from = x.jit.levels[x.jit.n-1].ctx.RSP
	} else if x.jit.rs != nil {
		from = x.rsStart()
	}
	from = x.jitRoom(from, 2)
	lv := x.jit.levels[x.jit.n]
	x.jit.n++
	inst := fn.Inst
	lv.inst, lv.cj = inst, cj
	chunk := x.nchunks[x.nc]
	lv.nc = x.nc
	lv.chunkBase = uintptr(unsafe.Pointer(&chunk[0]))
	lv.depth, lv.calls, lv.nsp = x.depth, len(x.calls), x.nsp
	ctx := &lv.ctx
	*ctx = wjit.Ctx{
		L:        uintptr(unsafe.Pointer(&l[0])),
		Links:    uintptr(unsafe.Pointer(&cj.links[0])),
		Globals:  inst.globalsAddr(),
		ChunkEnd: lv.chunkBase + uintptr(len(chunk))*8,
	}
	// A native call at entry j of the level is at depth x.depth+j: the
	// entries end where that would pass MaxCallDepth, and a call there
	// goes through Go, whose depth check traps.
	lv.rsBase = from
	lv.rsDepth = uintptr(max(MaxCallDepth-x.depth+1, 1)) * rsEntryBytes
	ctx.RSEnd = min(x.rsStart()+uintptr(len(x.jit.rs))*8, from+lv.rsDepth)
	i := x.rsIndex(from)
	x.jit.rs[i] = uint64(nat.retStub)
	x.jit.rs[i+1] = 0
	x.jit.rs[i+2] = wjit.Site(fn.Index, 0)
	ctx.RSP = from + rsEntryBytes

	raw, off := nat.raw, nat.goEntry
	for {
		if len(inst.Memories) > 0 {
			m := inst.Memories[0]
			ctx.MemBase = uintptr(m.Base())
			ctx.MemSize = m.Size()
			ctx.MemSizePtr = uintptr(unsafe.Pointer(&m.size))
		}
		ctx.Poll = int64(x.poll)
		cj.run.RLock()
		raw.EnterAt(off, unsafe.Pointer(ctx))
		cj.run.RUnlock()
		x.poll = int32(ctx.Poll)
		switch ctx.Exit {
		case wjit.ExitReturn:
			x.jit.n--
			return 0
		case wjit.ExitTrap:
			x.jitSync(lv, uint32(ctx.Arg1))
			trap(TrapCode(ctx.Arg0))
		case wjit.ExitPoll:
			x.pollContext()
		case wjit.ExitCall:
			callee := inst.Funcs[ctx.Arg0]
			frame, top := x.jitFrame(lv)
			x.jitSync(lv, uint32(ctx.Arg2))
			x.jitCall(lv, callee, callee.Sig, frame, int(ctx.Arg1), top)
			x.jitUnsync(lv)
		case wjit.ExitCallIndirect:
			ft := inst.Module.Types[ctx.Arg0].Func
			slot := int(ctx.Arg2)
			frame, top := x.jitFrame(lv)
			x.jitSync(lv, uint32(ctx.Arg3))
			callee := indirectTarget(inst, uint32(ctx.Arg0), uint32(ctx.Arg1), frame[slot+2*len(ft.Params)])
			x.jitCall(lv, callee, ft, frame, slot, top)
			x.jitUnsync(lv)
		case wjit.ExitTailCall, wjit.ExitTailIndirect:
			var callee *Function
			var ft *module.FuncType
			var slot int
			var at uint32
			frame, top := x.jitFrame(lv)
			if ctx.Exit == wjit.ExitTailCall {
				callee, slot, at = inst.Funcs[ctx.Arg0], int(ctx.Arg1), uint32(ctx.Arg2)
				ft = callee.Sig
			} else {
				ft = inst.Module.Types[ctx.Arg0].Func
				slot, at = int(ctx.Arg2), uint32(ctx.Arg3)
				x.jitSync(lv, at)
				callee = indirectTarget(inst, uint32(ctx.Arg0), uint32(ctx.Arg1), frame[slot+2*len(ft.Params)])
				x.jitUnsync(lv)
			}
			if r, o, done := x.jitTail(lv, callee, ft, frame, slot, top, at); done {
				return sigTail
			} else if r != nil {
				raw, off = r, o
				continue
			}
		case wjit.ExitOp:
			frame, top := x.jitFrame(lv)
			x.jitSync(lv, uint32(ctx.Arg4))
			x.nc, x.nsp = lv.nc, top
			x.jitOp(inst, opcode.Opcode(ctx.Arg0), uint32(ctx.Arg1), uint32(ctx.Arg2), frame, int(ctx.Arg3))
			x.jitUnsync(lv)
		default:
			panic(fmt.Sprintf("wasm jit: exit %d", ctx.Exit))
		}
		nf := inst.Code.get(uint32(ctx.ExitFn)).native.Load()
		raw, off = nf.raw, int(ctx.ExitAddr-nf.base)
	}
}

// jitFrame returns the slots of the frame that exited, and the position
// past it in the chunk, where a frame made for a call goes.
func (x *exec) jitFrame(lv *jitLevel) ([]uint64, int) {
	nslots := lv.inst.Code.get(uint32(lv.ctx.ExitFn)).nslots
	i := int((lv.ctx.L - lv.chunkBase) / 8)
	return x.nchunks[lv.nc][i : i+nslots : i+nslots], i + nslots
}

// jitSync makes the exec's call stack and depth say what the level's
// native frames are, the innermost at offset, for a call of Go to see and
// a trap to report.
func (x *exec) jitSync(lv *jitLevel, offset uint32) {
	rs := x.jit.rs
	first, last := x.rsIndex(lv.rsBase), x.rsIndex(lv.ctx.RSP)
	x.calls = x.calls[:lv.calls]
	for i := first; i < last; i += wjit.RSEntryWords {
		site := rs[i+2]
		callee := lv.inst.Funcs[site>>32]
		if i == first {
			x.calls[len(x.calls)-1].fn = callee
			continue
		}
		x.calls[len(x.calls)-1].offset = uint32(site)
		x.calls = append(x.calls, callSite{fn: callee})
	}
	x.calls[len(x.calls)-1].offset = offset
	x.depth = lv.depth + (last-first)/wjit.RSEntryWords - 1
}

// jitUnsync puts the exec's state back as the level found it.
func (x *exec) jitUnsync(lv *jitLevel) {
	x.calls = x.calls[:lv.calls]
	x.depth = lv.depth
	x.nc, x.nsp = lv.nc, lv.nsp
}

// narrow keeps a value's upper half clear for a 32-bit type, as native code
// keeps every i32 and f32 in a slot.
func narrow(v uint64, t module.ValueType) uint64 {
	if t.Kind == module.KindI32 || t.Kind == module.KindF32 {
		return uint64(uint32(v))
	}
	return v
}

// jitArgs reads a call's arguments of signature ft from the frame's slots
// from slot.
func (lv *jitLevel) jitArgs(ft *module.FuncType, frame []uint64, slot int) []Val {
	n := len(ft.Params)
	if cap(lv.args) < n {
		lv.args = make([]Val, n)
	}
	args := lv.args[:n]
	for i := range args {
		args[i] = Val{N: frame[slot+2*i]}
	}
	return args
}

// jitCall calls fn, of signature ft, for native code: its arguments in the
// frame's slots from slot, two apart, where its results go. A frame it
// makes goes at position top of the level's chunk.
func (x *exec) jitCall(lv *jitLevel, fn *Function, ft *module.FuncType, frame []uint64, slot, top int) {
	args := lv.jitArgs(ft, frame, slot)
	m := len(ft.Results)
	if cap(lv.res) < m {
		lv.res = make([]Val, m)
	}
	res := lv.res[:m]
	x.nc, x.nsp = lv.nc, top
	// A native call finding the return stack full came here: make it room.
	x.jitRoom(lv.ctx.RSP, 64)
	x.callGeneric(fn, args, res)
	for i, t := range ft.Results {
		frame[slot+2*i] = narrow(res[i].N, t)
	}
}

// jitTail makes a tail call of fn for the frame that exited: natively in
// its place where fn has native code and room, returning the code to enter;
// as the interpreter's tail call (done) if the frame is the one Go entered;
// else by calling fn and returning its results, the frame's code resumed
// at its return.
func (x *exec) jitTail(lv *jitLevel, fn *Function, ft *module.FuncType, frame []uint64, slot, top int, at uint32) (*ijit.RawCode, int, bool) {
	ctx := &lv.ctx
	args := lv.jitArgs(ft, frame, slot)
	// A tail call is a call: it counts toward the poll, as the
	// interpreter's do.
	if x.poll--; x.poll <= 0 {
		x.pollContext()
	}
	if fn.Inst == lv.inst && fn.Host == nil {
		cd := lv.inst.Code.get(fn.Index)
		start := top - len(frame)
		if nat := lv.cj.native(lv.inst.Code, fn.Index, cd); nat != nil && start+cd.nslots <= len(x.nchunks[lv.nc]) {
			callee := x.nchunks[lv.nc][start : start+cd.nslots]
			for i, a := range args {
				callee[i] = a.N
			}
			e := x.rsIndex(ctx.RSP) - wjit.RSEntryWords
			x.jit.rs[e+2] = wjit.Site(fn.Index, uint32(x.jit.rs[e+2]))
			return nat.raw, nat.goEntry, false
		}
	}
	if ctx.RSP-lv.rsBase == rsEntryBytes {
		// The frame Go entered: the interpreter's tail call makes it.
		x.tail = fn
		x.tailArgs = append(x.tailArgs[:0], args...)
		x.jit.n--
		return nil, 0, true
	}
	x.jitSync(lv, at)
	m := len(ft.Results)
	if cap(lv.res) < m {
		lv.res = make([]Val, m)
	}
	res := lv.res[:m]
	x.nc, x.nsp = lv.nc, top
	x.callGeneric(fn, args, res)
	for i, t := range ft.Results {
		frame[i] = narrow(res[i].N, t)
	}
	x.jitUnsync(lv)
	return nil, 0, false
}

// indirectTarget returns the function call_indirect of type typeIndex
// through table calls with index i, trapping as the interpreter does.
func indirectTarget(inst *Instance, typeIndex, table uint32, i uint64) *Function {
	if !inst.Module.Tables[table].Limits.Is64 {
		i = uint64(uint32(i))
	}
	want := inst.Module.Canonical[typeIndex]
	t := inst.Tables[table]
	if i >= uint64(len(t.Elems)) {
		trap(TrapTableOutOfBounds)
	}
	e := t.Elems[i]
	if e.IsNull() {
		trap(TrapNullFunc)
	}
	fn := e.Func()
	if fn.Type != want && !module.IsCanonicalSubtype(fn.Type, want) {
		trap(TrapFuncSigMismatch)
	}
	return fn
}

// jitOp runs instruction op for native code, with immediates x and y, its
// operands in the frame's slots from slot, two apart, its result left in
// the first.
func (x *exec) jitOp(inst *Instance, op opcode.Opcode, a, b uint32, frame []uint64, slot int) {
	m := inst.Module
	arg := func(i int, is64 bool) uint64 {
		if is64 {
			return frame[slot+2*i]
		}
		return uint64(uint32(frame[slot+2*i]))
	}
	mem64 := func(i uint32) bool { return m.Memories[i].Limits.Is64 }
	tab64 := func(i uint32) bool { return m.Tables[i].Limits.Is64 }
	put := func(v uint64, is64 bool) {
		if !is64 {
			v = uint64(uint32(v))
		}
		frame[slot] = v
	}
	switch op {
	case opcode.MemorySize:
		put(inst.Memories[a].Pages(), mem64(a))
	case opcode.MemoryGrow:
		put(uint64(inst.Memories[a].Grow(arg(0, mem64(a)))), mem64(a))
	case opcode.MemoryInit:
		// a is the segment, b the memory.
		dst, src, cnt := arg(0, mem64(b)), arg(1, false), arg(2, false)
		data := inst.datas[a]
		buf := inst.Memories[b].bytes()
		checkRange(src, cnt, uint64(len(data)), TrapMemOutOfBounds)
		checkRange(dst, cnt, uint64(len(buf)), TrapMemOutOfBounds)
		copy(buf[dst:dst+cnt], data[src:src+cnt])
	case opcode.DataDrop:
		inst.datas[a] = nil
	case opcode.MemoryCopy:
		d64, s64 := mem64(a), mem64(b)
		dst, src, cnt := arg(0, d64), arg(1, s64), arg(2, d64 && s64)
		db, sb := inst.Memories[a].bytes(), inst.Memories[b].bytes()
		checkRange(src, cnt, uint64(len(sb)), TrapMemOutOfBounds)
		checkRange(dst, cnt, uint64(len(db)), TrapMemOutOfBounds)
		copy(db[dst:dst+cnt], sb[src:src+cnt])
	case opcode.MemoryFill:
		is64 := mem64(a)
		dst, val, cnt := arg(0, is64), byte(arg(1, false)), arg(2, is64)
		buf := inst.Memories[a].bytes()
		checkRange(dst, cnt, uint64(len(buf)), TrapMemOutOfBounds)
		s := buf[dst : dst+cnt]
		for i := range s {
			s[i] = val
		}
	case opcode.TableInit:
		// a is the segment, b the table.
		dst, src, cnt := arg(0, tab64(b)), arg(1, false), arg(2, false)
		elems := inst.elems[a]
		t := inst.Tables[b]
		checkRange(src, cnt, uint64(len(elems)), TrapTableOutOfBounds)
		checkRange(dst, cnt, uint64(len(t.Elems)), TrapTableOutOfBounds)
		copy(t.Elems[dst:dst+cnt], elems[src:src+cnt])
	case opcode.ElemDrop:
		inst.elems[a] = nil
	case opcode.TableCopy:
		d64, s64 := tab64(a), tab64(b)
		dst, src, cnt := arg(0, d64), arg(1, s64), arg(2, d64 && s64)
		de, se := inst.Tables[a].Elems, inst.Tables[b].Elems
		checkRange(src, cnt, uint64(len(se)), TrapTableOutOfBounds)
		checkRange(dst, cnt, uint64(len(de)), TrapTableOutOfBounds)
		copy(de[dst:dst+cnt], se[src:src+cnt])
	case opcode.TableSize:
		put(inst.Tables[a].Size(), tab64(a))
	default:
		panic("wasm jit: no Go operation " + op.Name())
	}
}
