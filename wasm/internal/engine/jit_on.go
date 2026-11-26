//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
	"weak"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	wopt "github.com/go-quickjs/go-quickjs/wasm/internal/jit/opt"
	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
	"github.com/go-quickjs/go-quickjs/wasm/internal/opcode"
)

const jitBuilt = true

func jitAvailable() bool { return ijit.Supported() }

// jitCodeIDs counts the codeJITs made, for their ids.
var jitCodeIDs atomic.Uint64

// codeJIT is a Code's native code: its functions' code in an arena of its
// own, and the link table native calls go through.
type codeJIT struct {
	// test is its Code's (codeTest), nil but in a test's.
	test *codeTest
	// id tells the code's levels on an exec from others' (codeLevels).
	id        uint64
	threshold int32
	// osrThreshold is the number of back edges after which a call running
	// in the interpreter goes on in native code; math.MaxInt32 is never.
	osrThreshold atomic.Int32
	// osrCalls has calls of a function with loops, made from Go or the
	// interpreter, run in the interpreter and enter the native code at
	// their first back edge: osrFirst its first call (WASM_JIT=eager),
	// osrEvery every call (WASM_JIT=osr, EnterLoopsForTest).
	osrCalls int8
	// mu serializes compiles, and what drops code (jit_mem.go).
	mu sync.Mutex
	// run is held for reading while native code of the Code runs, and for
	// writing while code is placed or dropped: placing makes pages of the
	// arena writable, which no thread may be running then. Code placed
	// apart from the running code (placeReady) does not take it.
	run   sync.RWMutex
	arena *ijit.Arena
	// links holds each function's native entry, or 0; native code reads
	// it, so it is written atomically and never moves.
	links []uintptr
	// frames holds each function's frame in bytes once it has native code,
	// which a native call through a table or a reference checks its room
	// by (Ctx.Frames): for a function that holds references one past any
	// chunk's end. rframes holds both of its frame's sizes, the numeric
	// slots' bytes in the low word and the reference slots' its native code
	// uses in the high one (Ctx.RFrames), which the baseline's calls check
	// a callee that holds references by. Both are written atomically,
	// before the link.
	frames, rframes []uint64
	// raws are the codes placed, which the Code holds until they are
	// dropped or it is collected.
	raws []*placedCode
	// instrs holds the code of each instruction native code leaves to Go
	// (ExitInstr), by its offset in the module and its operands' slots
	// (instrExitKey, instrExit).
	instrs sync.Map
	// catches says a function of the code has catch clauses: exits are
	// handled under a recover that takes exceptions to them.
	catches atomic.Bool
	// optThreshold is the number of calls of a function's baseline code
	// -- a poll its code exits for counting optPollWeight of them -- after
	// which the optimizing tier compiles it; 0 has the optimizing tier
	// compile each function it accepts in the baseline's place, and a
	// negative one turns it off. counters are the functions' countdowns,
	// which their baseline code decrements at its entry; they never move.
	optThreshold atomic.Int32
	counters     []int64
	// bg is the state of the optimizing tier's compiles in the background
	// (jit_bg.go).
	bg optBackground

	// budget is what the code is charged to (JITBudget), and codeBytes,
	// recordBytes and tableBytes what it holds (JITMemory): budget and
	// tableBytes change under mu.
	budget                 *JITBudget
	codeBytes, recordBytes atomic.Int64
	tableBytes             int64
	// fns are the states of the functions compiled, by index, for what
	// drops code; reclaimedAt is the epoch of the last pass that did, and
	// dropped counts the functions whose code it dropped. They change under
	// mu.
	fns         []*fnJIT
	reclaimedAt uint64
	dropped     atomic.Int64
	// inline is the sites of the calls the optimizing tier inlined
	// (jit_inline.go).
	inline inlineSites
}

// optPollWeight is how many calls a poll exit of a function's baseline
// code counts toward the optimizing tier: the poll counter ran out in its
// loops or its calls.
const optPollWeight = 250

// nativeFirst reports whether native code runs the code's functions from
// their first calls, which then need no body for the interpreter.
func (c *Code) nativeFirst() bool {
	cj := c.jit.Load()
	return cj != nil && cj.threshold == 0
}

// nativeFn is a function's native code.
type nativeFn struct {
	raw *ijit.RawCode
	// place is the code's place in the arena, and base and end its first
	// byte and the byte past its last.
	place     *placedCode
	base, end uintptr
	// entry is the offset of the entry native calls make, which the link
	// table holds, and goEntry Go's.
	entry, goEntry int
	retStub        uintptr
	nslots         int
	// loops are the offsets of the loops' entries in the code, by the
	// loop instruction's offset in the module.
	loops map[uint32]int
	// handlers are the try_tables' catch clauses (wjit.Handler).
	handlers []wjit.Handler
	// opt says the code is the optimizing tier's, and prev is the
	// baseline code it replaced, if any: frames that began in it run on
	// in it, and calls running in the interpreter enter its loops.
	opt  bool
	prev *nativeFn
}

// holding is the code, n's or a code it replaced, that holds address
// addr: where a frame that exited resumes.
func (n *nativeFn) holding(addr uintptr) *nativeFn {
	for p := n; p != nil; p = p.prev {
		if addr >= p.base && addr < p.end {
			return p
		}
	}
	return n
}

// loopCode is the code whose loops' entries a call running in the
// interpreter enters: n's, or the baseline code it replaced.
func (n *nativeFn) loopCode() *nativeFn {
	for p := n; p != nil; p = p.prev {
		if len(p.loops) > 0 {
			return p
		}
	}
	return n
}

// jitInstance is what an instance's native code reads of it: the
// addresses of its globals, of its functions and of its tables.
type jitInstance struct {
	globals, funcs, tables atomic.Pointer[[]uintptr]
}

// jitLevel is one entry of native code from Go, and the native calls it
// makes until it returns: its context, and the state of the exec it
// restores after each exit's work.
type jitLevel struct {
	ctx  wjit.Ctx
	inst *Instance
	cj   *codeJIT
	// x is the exec the level runs on, for the allocation stub's Go.
	x *exec
	// chain is the exec's chain of cj's levels the level is in, and below
	// the level of it the level was entered above, nil for the first.
	chain *codeLevels
	below *jitLevel
	// rsBase is the address of the level's first return stack entry, the
	// one for the frame Go entered; rsDepth is how many bytes of entries
	// past it the depth limit allows.
	rsBase, rsDepth uintptr
	// nc and chunkBase are the stack chunk the frames are in, rc and
	// rchunkBase the reference stack's.
	nc, rc                int
	chunkBase, rchunkBase uintptr
	// depth, calls, nsp and rsp are the exec's when the level was entered,
	// r0 the reference slots of the frame Go entered.
	depth, calls, nsp, rsp int
	r0                     uintptr
	args, res              []Val
	// mems is the table of the instance's memories native code reads
	// (Ctx.Mems), filled before every entry.
	mems []uint64
	// frame is the frame an ExitInstr runs the interpreter's code in.
	frame frame
	// rargs is the exit's Ctx.RArgs, taken when Go begins its work.
	rargs uint64
	// pools is the table of pools of new structs the level's code takes
	// from (Ctx.Pools), nil until its first ExitAlloc; ownPools says the
	// level took it from the instance, rather than sharing a level's below.
	pools    *gcPools
	ownPools bool
}

// jitLayout is where the engine's objects keep what native code reads.
var jitLayout = &wjit.Layout{
	TableElems: int32(unsafe.Offsetof(Table{}.Elems)),
	I31Mark:    uint64(uintptr(unsafe.Pointer(&i31Mark))),
	Kind:       int32(unsafe.Offsetof(header{}.kind)),
	KindStruct: uint8(ObjStruct),
	KindArray:  uint8(ObjArray),
	RTT:        int32(unsafe.Offsetof(header{}.rtt)),
	Supers:     int32(unsafe.Offsetof(rtt{}.supers)),
	ArrayLen:   int32(unsafe.Offsetof(Array{}.n)),
	ArrayElems: int32(unsafe.Offsetof(Array{}.e)),
	FuncType:   int32(unsafe.Offsetof(Function{}.Type)),
	FuncInst:   int32(unsafe.Offsetof(Function{}.Inst)),
	FuncIndex:  int32(unsafe.Offsetof(Function{}.Index)),
	PoolCount:  int32(unsafe.Offsetof(gcPool{}.n)),
	PoolObjs:   int32(unsafe.Offsetof(gcPool{}.objs)),
	ArrayBlock: int32(arrayHeader),
	ArrayClass: arrayClassOf[:],
}

// A type's supertypes are 4 bytes each, as native code reads them.
var _ [unsafe.Sizeof(module.CanonicalIndex(0)) - 4]struct{}

// jitWriteBarrier and jitStoreRef are the write barrier's flag and store
// native code is compiled with: the runtime's (internal/jit), or a test's.
// WASM_JIT_BARRIER, for tests, has the code see the collector always
// marking ("marking"), so that every store of a pointer word goes through
// the runtime's barrier, and "clobber" has that store first fill every
// register native code may keep with junk, so that code that keeps one
// across the store without saving it goes wrong.
var jitWriteBarrier, jitStoreRef = func() (uint64, uint64) {
	switch os.Getenv("WASM_JIT_BARRIER") {
	case "marking":
		return uint64(uintptr(unsafe.Pointer(&jitAlwaysMarking))), ijit.StoreRef()
	case "clobber":
		return uint64(uintptr(unsafe.Pointer(&jitAlwaysMarking))), ijit.StoreRefClobber()
	}
	return ijit.WriteBarrier(), ijit.StoreRef()
}()

// jitExitAll and jitFeatures are what the process's code is compiled with,
// for tests: WASM_JIT_EXITS=all has native code run every instruction an
// exit can run in Go (wjit.Request.ExitAll), and WASM_JIT_CPU=base has it
// compiled for a CPU without POPCNT, SSE4.1 and the SIMD its vectors take
// (wjit.Request.Features).
var (
	jitExitAll  = os.Getenv("WASM_JIT_EXITS") == "all"
	jitFeatures = func() *wjit.Features {
		if os.Getenv("WASM_JIT_CPU") != "base" {
			return nil
		}
		f := wjit.CPU
		f.Popcnt, f.Round, f.SIMD = false, false, false
		return &f
	}()
)

// jitAlwaysMarking is a write barrier's flag that is always set.
var jitAlwaysMarking uint8 = 1

// setTest makes t what a test compiles and runs c with, its native code's
// state's too.
func (c *Code) setTest(t *codeTest) {
	c.test = t
	if cj := c.jit.Load(); cj != nil {
		cj.test = t
	}
}

// codeJITTest is codeTest's for the JIT: what a test compiles a Code's
// native code with in place of the process's.
type codeJITTest struct {
	// writeBarrier and storeRef, if not 0, are the write barrier's flag
	// and store the code is compiled with (jitWriteBarrier, jitStoreRef);
	// layout, if set, is where the engine's objects keep what it reads
	// (jitLayout); allocStub, if not 0, the allocation stub's address
	// (jitAllocStubAddr).
	writeBarrier, storeRef, allocStub uint64
	layout                            *wjit.Layout
	// allocHook, if set, is called by the allocation stub's Go
	// (jitAllocStub) as it begins; noAllocStub compiles code that exits
	// for what the stub makes.
	allocHook   func()
	noAllocStub bool
	// exitAll has the code run every instruction an exit can run in Go
	// (wjit.Request.ExitAll); features, if set, are the CPU's features the
	// code is compiled for (wjit.Request.Features).
	exitAll  bool
	features *wjit.Features
}

// jitExec is an exec's native state: the levels entered, and the native
// return stack they share.
type jitExec struct {
	levels []*jitLevel
	n      int // the number of levels running
	rs     []uint64
	// entries is jitSync's list of a level's return stack entries.
	entries []int
	// codes are the exec's chains of the levels of each Code it entered
	// lately (codeLevels), which a pass of a Code that drops code reads
	// (codeJIT.pins), and last the one entered last.
	codes []*codeLevels
	last  *codeLevels
	// mu orders what the pass reads of the exec -- codes, the return stack
	// and where its levels' entries are in it -- with the exec's changes to
	// codes and moves of the return stack, which it makes holding mu, and
	// only those: the exec changes its levels holding the run lock of their
	// Code's for reading, which no pass of that Code shares (jit_mem.go).
	mu sync.Mutex
}

// codeLevels is the chain of the levels of one Code's running on an exec,
// the innermost on top, each linked to the one below it (jitLevel.below).
// The exec changes it holding the Code's run lock for reading.
type codeLevels struct {
	// id is the Code's (codeJIT.id).
	id  uint64
	top *jitLevel
}

// jitChainsKept is the number of chains an exec keeps before it lets go
// of those with no level running: those of the Codes it entered lately,
// which it then finds its chain of without making one.
const jitChainsKept = 16

func (x *exec) jitLevel() int { return x.jit.n }

// setJITLevel ends the levels past the first n, as a panic out of them
// does.
func (x *exec) setJITLevel(n int) {
	if x.jit.n > n {
		x.endLevels(n)
	}
}

// endLevels is setJITLevel's loop.
func (x *exec) endLevels(n int) {
	for x.jit.n > n {
		lv := x.jit.levels[x.jit.n-1]
		lv.cj.run.RLock()
		x.popLevel(lv)
		lv.cj.run.RUnlock()
	}
}

// pushLevel begins lv as the innermost level, of cj's, in the exec's chain
// ch of cj's levels. cj's run lock is held for reading.
func (x *exec) pushLevel(lv *jitLevel, cj *codeJIT, ch *codeLevels) {
	lv.cj, lv.chain, lv.below, lv.x = cj, ch, ch.top, x
	ch.top = lv
	x.jit.n++
}

// popLevel ends lv, the innermost level. Its Code's run lock is held for
// reading.
func (x *exec) popLevel(lv *jitLevel) {
	if lv.pools != nil {
		lv.dropPools()
	}
	lv.chain.top = lv.below
	x.jit.n--
}

// chainOf returns the exec's chain of cj's levels, made if it has none,
// where the last it entered is another Code's.
func (x *exec) chainOf(cj *codeJIT) *codeLevels {
	for _, ch := range x.jit.codes {
		if ch.id == cj.id {
			x.jit.last = ch
			return ch
		}
	}
	ch := &codeLevels{id: cj.id}
	x.jit.mu.Lock()
	if len(x.jit.codes) >= jitChainsKept {
		keep := x.jit.codes[:0]
		for _, c := range x.jit.codes {
			if c.top != nil {
				keep = append(keep, c)
			}
		}
		clear(x.jit.codes[len(keep):])
		x.jit.codes = keep
	}
	x.jit.codes = append(x.jit.codes, ch)
	x.jit.mu.Unlock()
	x.jit.last = ch
	return ch
}

// jitEnvOSR is which calls WASM_JIT has enter loops' native code at their
// first back edge (codeJIT.osrCalls): "eager" the first, "osr" every one.
var jitEnvOSR = map[string]int8{"eager": osrFirst, "osr": osrEvery}[os.Getenv("WASM_JIT")]

// jitCode, for profiles, is WASM_JIT_CODE's file, or nil: each code placed
// is a line of JSON -- its function and tier, its address, bytes and the
// compiler's marks and listing -- which internal/jit/verify's jitprof joins
// with a CPU profile's samples in native code.
var jitCode = func() *jitCodeLog {
	path := os.Getenv("WASM_JIT_CODE")
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		panic(fmt.Sprintf("WASM_JIT_CODE: %v", err))
	}
	return &jitCodeLog{enc: json.NewEncoder(f)}
}()

type jitCodeLog struct {
	mu  sync.Mutex
	enc *json.Encoder
	seq int
}

// log writes the line of function fn's code, of tier tier, placed at raw.
func (l *jitCodeLog) log(fn uint32, tier string, raw *ijit.RawCode, code *wjit.Code) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.enc.Encode(struct {
		Seq          int
		Name, Script string
		Line         int
		Addr         uint64
		Text         int
		Bytes        []byte
		Marks        []wjit.Mark
		Listing      string
	}{l.seq, fmt.Sprintf("f%d %s", fn, tier), "wasm", 0, uint64(raw.Base()), len(code.Bytes), code.Bytes, code.Marks, code.Listing})
}

// optEnv is the optimizing tier's threshold WASM_JIT_OPT sets -- a number
// of calls, "on" for DefaultOptThreshold, "eager" for 0, "off" for -1 --
// or WASM_JIT=opt, which has every function compiled by it at its first
// call; -2 is none.
var optEnv = func() int {
	if os.Getenv("WASM_JIT") == "opt" {
		return 0
	}
	switch v := os.Getenv("WASM_JIT_OPT"); v {
	case "":
		return -2
	case "off":
		return -1
	case "eager":
		return 0
	case "on":
		return DefaultOptThreshold
	default:
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
		return -2
	}
}()

// jitEnv is WASM_JIT's threshold, or -1 for none.
var jitEnv = func() int {
	switch v := os.Getenv("WASM_JIT"); v {
	case "", "off", "0":
		return -1
	case "eager", "osr", "opt":
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

// osrEnv is WASM_JIT_OSR's threshold, math.MaxInt32 for "off", or -1 for
// none.
var osrEnv = func() int {
	switch v := os.Getenv("WASM_JIT_OSR"); v {
	case "":
		return -1
	case "off":
		return math.MaxInt32
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
		if jitEnvOSR != 0 {
			c.setOSRCalls(jitEnvOSR)
		}
	}
}

const (
	osrFirst = 1 + iota
	osrEvery
)

func (c *Code) enterLoopsForTest() { c.setOSRCalls(osrEvery) }

// setOSRCalls sets which calls of a function with loops enter its native
// code at their first back edge (codeJIT.osrCalls), or none for 0.
func (c *Code) setOSRCalls(mode int8) {
	if cj := c.jit.Load(); cj != nil {
		if mode != 0 {
			c.setOSRThreshold(0)
		}
		cj.osrCalls = mode
	}
}

func (c *Code) setOptThreshold(n int) {
	if cj := c.jit.Load(); cj != nil && wopt.Available {
		cj.optThreshold.Store(int32(max(min(n, math.MaxInt32), -1)))
	}
}

func (c *Code) setOSRThreshold(n int) {
	cj := c.jit.Load()
	if cj == nil {
		return
	}
	if n < 0 {
		n = math.MaxInt32
	}
	cj.osrThreshold.Store(int32(min(n, math.MaxInt32)))
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
	n := max(len(c.Module.Functions), 1)
	cj := &codeJIT{test: c.test, id: jitCodeIDs.Add(1), threshold: int32(threshold), arena: ijit.NewReusingArena(), links: make([]uintptr, n), frames: make([]uint64, n), rframes: make([]uint64, n), counters: make([]int64, n),
		fns: make([]*fnJIT, n), tableBytes: tableBytes(n)}
	cj.inline.base = wjit.InlineBase(len(c.Module.Bytes))
	cj.joinBudget(NewJITBudget(jitMemEnv))
	cj.optThreshold.Store(-1)
	if wopt.Available {
		cj.optThreshold.Store(DefaultOptThreshold)
	}
	if optEnv >= -1 && wopt.Available {
		cj.optThreshold.Store(int32(optEnv))
	}
	cj.osrThreshold.Store(DefaultOSRThreshold)
	if osrEnv >= 0 {
		cj.osrThreshold.Store(int32(osrEnv))
	}
	cj.bg.mode.Store(optBackgroundDefault())
	cj.bg.owner = weak.Make(c)
	runtime.AddCleanup(c, func(cj *codeJIT) { cj.close() }, cj)
	c.jit.Store(cj)
	return true
}

// close gives the code back, once the Code is unreachable.
func (cj *codeJIT) close() {
	optWorkers.drop(cj)
	cj.mu.Lock()
	defer cj.mu.Unlock()
	cj.bg.closed = true
	for _, pc := range cj.raws {
		pc.dropped.Store(true)
		pc.raw.Close()
	}
	cj.raws = nil
	cj.leaveBudget()
}

// native returns function idx's native code, compiling it once it has been
// called often enough, or nil.
func (cj *codeJIT) native(c *Code, idx uint32, cd *compiled) *nativeFn {
	if nat := cd.native.Load(); nat != nil {
		if atomic.LoadUintptr(&cj.links[idx]) == 0 {
			cj.relink(idx, cd)
		}
		if cj.osrCalls == osrEvery && len(nat.loopCode().loops) > 0 {
			return nil
		}
		return nat
	}
	if cd.refused.Load() || cd.hot.Add(1) <= cj.threshold || cj.waitsForRoom(cd) {
		return nil
	}
	nat := cj.compile(c, idx, cd)
	if nat != nil && cj.osrCalls != 0 && len(nat.loops) > 0 {
		// The call goes on in the interpreter, into the loops' entries.
		return nil
	}
	return nat
}

func (cj *codeJIT) compile(c *Code, idx uint32, cd *compiled) (nat *nativeFn) {
	cj.mu.Lock()
	defer cj.mu.Unlock()
	if nat := cd.native.Load(); nat != nil || cd.refused.Load() || cj.waitsForRoom(cd) {
		return nat
	}
	cj.fns[idx] = cd.fnJIT
	if !cj.mayFit(c, idx, cd, false) {
		cj.noRoom(cd, false, 0)
		return nil
	}
	if cj.optThreshold.Load() == 0 {
		if nat := cj.compileOpt(c, idx, cd, nil); nat != nil {
			return nat
		}
	}
	var counter uint64
	if t := cj.optThreshold.Load(); t > 0 && !cd.optDone.Load() {
		atomic.StoreInt64(&cj.counters[idx], int64(t))
		counter = uint64(uintptr(unsafe.Pointer(&cj.counters[idx])))
	}
	start := time.Now()
	code, err := compileNative(c, idx, cd, counter)
	countTime(&jitStats.baseTime, start)
	if err != nil {
		cd.refused.Store(true)
		reason := "internal"
		if r, ok := err.(*wjit.Refusal); ok {
			reason = r.Reason
		}
		countRefused(reason)
		return nil
	}
	placeStart := time.Now()
	pc, why := cj.placeCharged(code.Bytes, recordBytes(code.Loops, code.Handlers))
	countTime(&jitStats.placeTime, placeStart)
	switch why {
	case RefuseBudget:
		cj.noRoom(cd, false, placedNeed(code.Bytes, recordBytes(code.Loops, code.Handlers)))
		return nil
	case "unplaced":
		cd.refused.Store(true)
		countRefused("unplaced")
		return nil
	}
	jitCode.log(idx, "base", pc.raw, code)
	nat = newNative(pc, code, cd)
	nat.loops, nat.handlers = code.Loops, code.Handlers
	if len(code.Handlers) > 0 {
		cj.catches.Store(true)
	}
	cd.used.Store(cj.epoch())
	cd.native.Store(nat)
	cj.setFrame(idx, cd)
	atomic.StoreUintptr(&cj.links[idx], nat.base+uintptr(code.Entry))
	countCompiled()
	return nat
}

// newNative returns the record of code, placed at pc, of the function
// whose record is cd.
func newNative(pc *placedCode, code *wjit.Code, cd *compiled) *nativeFn {
	base := pc.raw.Base()
	return &nativeFn{raw: pc.raw, place: pc, base: base, end: base + uintptr(pc.raw.Size()), entry: code.Entry, goEntry: code.GoEntry,
		retStub: base + uintptr(code.ReturnStub), nslots: cd.nslots}
}

// relink stores function idx's native entry in the link table again, once
// a pass that drops code has cleared it to see whether the function is
// still called (jit_mem.go), and notes the call.
func (cj *codeJIT) relink(idx uint32, cd *compiled) {
	cj.run.RLock()
	if nat := cd.native.Load(); nat != nil && !nat.place.dropped.Load() {
		cd.used.Store(cj.epoch())
		atomic.StoreUintptr(&cj.links[idx], nat.base+uintptr(nat.entry))
	}
	cj.run.RUnlock()
}

// compileOpt compiles function idx with the optimizing tier, placing the
// code in the place of prev, its baseline code if it has one, in the link
// table; it returns nil where the tier refuses the function, which keeps
// prev. cj.mu is held.
func (cj *codeJIT) compileOpt(c *Code, idx uint32, cd *compiled, prev *nativeFn) *nativeFn {
	if cd.optDone.Load() {
		return nil
	}
	cd.optDone.Store(true)
	if !c.withinBound(idx, cd) {
		countOptRefused(wjit.RefuseFrame)
		return nil
	}
	cj.fns[idx] = cd.fnJIT
	if !cj.mayFit(c, idx, cd, true) {
		// Tried again once it has counted as many calls again.
		cd.optDone.Store(false)
		cj.noRoom(cd, true, 0)
		return nil
	}
	req := optRequest(c, cj, idx, cd)
	req.MaxWorkBytes = cj.workRoom()
	start := time.Now()
	code, err := wopt.Compile(req)
	countTime(&jitStats.optTime, start)
	if err != nil {
		reason := "internal"
		if r, ok := err.(*wjit.Refusal); ok {
			if r.Reason == wjit.RefuseWork {
				// No room for its working sets: as for its code.
				cd.optDone.Store(false)
				cj.noRoom(cd, true, refusedWork(c, idx, r))
				return nil
			}
			reason = r.Reason
		}
		countOptRefused(reason)
		return nil
	}
	placeStart := time.Now()
	pc, why := cj.placeCharged(code.Bytes, recordBytes(nil, code.Handlers))
	countTime(&jitStats.placeTime, placeStart)
	switch why {
	case RefuseBudget:
		cd.optDone.Store(false)
		cj.noRoom(cd, true, placedNeed(code.Bytes, recordBytes(nil, code.Handlers)))
		return nil
	case "unplaced":
		countOptRefused("unplaced")
		return nil
	}
	return cj.publishOpt(idx, cd, pc, code, prev)
}

// publishOpt makes code, placed at pc, function idx's native code in the
// place of prev, its baseline code if it has one, and in the link table.
// cj.mu is held.
func (cj *codeJIT) publishOpt(idx uint32, cd *compiled, pc *placedCode, code *wjit.Code, prev *nativeFn) *nativeFn {
	cj.fns[idx] = cd.fnJIT
	jitCode.log(idx, "opt", pc.raw, code)
	nat := newNative(pc, code, cd)
	nat.opt, nat.prev = true, prev
	nat.handlers = code.Handlers
	if len(code.Handlers) > 0 {
		cj.catches.Store(true)
	}
	cd.used.Store(cj.epoch())
	cd.native.Store(nat)
	cj.setFrame(idx, cd)
	atomic.StoreUintptr(&cj.links[idx], nat.base+uintptr(code.Entry))
	countOptimized()
	return nat
}

// setFrame records function idx's frame for native calls through a table
// or a reference (codeJIT.frames, rframes), before its link is stored.
func (cj *codeJIT) setFrame(idx uint32, cd *compiled) {
	b, r := uint64(8*cd.nslots), uint64(16*cd.nativeRefSlots())
	atomic.StoreUint64(&cj.rframes[idx], r<<32|b)
	if r > 0 {
		// Past any chunk's end: such a call checks the reference slots'
		// room too (rframes), or goes through Go.
		b = math.MaxUint32
	}
	atomic.StoreUint64(&cj.frames[idx], b)
}

// tierUp has function idx, whose baseline code counted enough calls,
// compiled by the optimizing tier, returning the optimized code, or nil
// where the tier refuses it -- or, in the background, nil until a worker
// has published it, the call going on in the baseline code.
func (cj *codeJIT) tierUp(c *Code, idx uint32, cd *compiled) *nativeFn {
	if cj.inBackground() {
		if nat := cd.native.Load(); nat != nil && nat.opt {
			return nat
		}
		cj.tierUpLater(idx, cd)
		return nil
	}
	cj.mu.Lock()
	defer cj.mu.Unlock()
	if nat := cd.native.Load(); nat != nil && nat.opt {
		return nat
	}
	nat := cj.compileOpt(c, idx, cd, cd.native.Load())
	if nat == nil {
		n := int64(math.MaxInt64)
		if !cd.optDone.Load() {
			// No room for it now: tried again once it has counted as many
			// calls again.
			n = int64(max(cj.optThreshold.Load(), 1))
		}
		atomic.StoreInt64(&cj.counters[idx], n)
	}
	return nat
}

// loopsFor compiles baseline code for function idx, whose native code is
// the optimizing tier's alone, for its loops' entries: it is kept as the
// code the optimized code replaced, out of the link table. It returns the
// function's native code, or nil where the baseline refuses it.
func (cj *codeJIT) loopsFor(c *Code, idx uint32, cd *compiled) *nativeFn {
	cj.mu.Lock()
	defer cj.mu.Unlock()
	nat := cd.native.Load()
	if nat == nil || !nat.opt || nat.prev != nil {
		return nat
	}
	if !cj.mayFit(c, idx, cd, false) {
		return nil
	}
	start := time.Now()
	code, err := compileNative(c, idx, cd, 0)
	countTime(&jitStats.baseTime, start)
	if err != nil {
		return nil
	}
	placeStart := time.Now()
	pc, why := cj.placeCharged(code.Bytes, recordBytes(code.Loops, code.Handlers))
	countTime(&jitStats.placeTime, placeStart)
	if why != "" {
		return nil
	}
	jitCode.log(idx, "loops", pc.raw, code)
	if len(code.Handlers) > 0 {
		cj.catches.Store(true)
	}
	base := newNative(pc, code, cd)
	base.loops, base.handlers = code.Loops, code.Handlers
	n := *nat
	n.prev = base
	cd.native.Store(&n)
	countCompiled()
	return &n
}

// sample counts an exit of function fn's baseline code toward the
// optimizing tier, as weight calls, compiling it there if the count runs
// out: the call that exited goes on in the baseline code, and later calls
// enter the optimized code. A poll counts optPollWeight; a call or an
// instruction Go makes for the code counts one, as costly as a call and
// what the optimized code makes natively in part (call_indirect,
// memory.copy, memory.fill) -- so that a function called once whose loop
// calls through Go, the polls then Go's, is optimized for its next call.
func (cj *codeJIT) sample(c *Code, fn uint32, weight int64) {
	if cj.optThreshold.Load() <= 0 {
		return
	}
	cd := c.get(fn)
	if nat := cd.native.Load(); nat == nil || nat.opt || cd.optDone.Load() {
		return
	}
	if atomic.AddInt64(&cj.counters[fn], -weight) < 0 {
		cj.tierUp(c, fn, cd)
	}
}

// compileNative compiles function idx; a panic of the compiler's is a
// refusal, and the function stays in the interpreter. counter is the
// address of its countdown to the optimizing tier, or 0.
func compileNative(c *Code, idx uint32, cd *compiled, counter uint64) (code *wjit.Code, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("wasm jit: compiling function %d: %v", idx, p)
		}
	}()
	if !c.withinBound(idx, cd) {
		return nil, &wjit.Refusal{Reason: wjit.RefuseFrame, Detail: "a frame larger than validation's bound"}
	}
	if t := c.test; t != nil && t.refuse != nil && t.refuse(idx) {
		return nil, &wjit.Refusal{Reason: refuseTest}
	}
	return wjit.Compile(request(c, idx, cd, counter))
}

// request is the request to compile function idx.
func request(c *Code, idx uint32, cd *compiled, counter uint64) *wjit.Request {
	wb, sr, lay, as := jitWriteBarrier, jitStoreRef, jitLayout, jitAllocStubAddr
	exitAll, feat := jitExitAll, jitFeatures
	if t := c.test; t != nil {
		exitAll = exitAll || t.jit.exitAll
		if t.jit.features != nil {
			feat = t.jit.features
		}
		if t.jit.writeBarrier != 0 {
			wb = t.jit.writeBarrier
		}
		if t.jit.storeRef != 0 {
			sr = t.jit.storeRef
		}
		if t.jit.allocStub != 0 {
			as = t.jit.allocStub
		}
		if t.jit.noAllocStub {
			as = 0
		}
		if t.jit.layout != nil {
			lay = t.jit.layout
		}
	}
	return &wjit.Request{
		Module:         c.Module,
		Func:           idx,
		FrameSlots:     cd.nslots,
		FrameSlotsOf:   func(f uint32) int { n, _ := c.calleeFrame(f); return n },
		FrameRefSlots:  cd.nativeRefSlots(),
		RefSlotsOf:     func(f uint32) int { _, n := c.calleeFrame(f); return n },
		FrameLocals:    cd.nlocals,
		FrameRefLocals: cd.rlocals,
		WriteBarrier:   wb,
		StoreRef:       sr,
		AllocStub:      as,
		Layout:         lay,
		CaughtSlot:     cd.caught,
		TierCounter:    counter,
		Marks:          jitCode != nil,
		ExitAll:        exitAll,
		Features:       feat,
	}
}

// optRequest is the request to compile function idx with the optimizing
// tier, which may inline calls, the Code giving their sites' offsets.
func optRequest(c *Code, cj *codeJIT, idx uint32, cd *compiled) *wjit.Request {
	req := request(c, idx, cd, 0)
	if jitInline {
		req.InlineSite = cj.inline.site
	}
	return req
}

// calleeFrame is the numeric slots of function f's frame, and the
// reference slots its native code uses, as native code calling f sizes
// them: validation's bound (frameBound), which needs no compile of f, or
// where there is none f's compiled body's.
func (c *Code) calleeFrame(f uint32) (nslots, refSlots int) {
	if n, r, ok := c.frameBound(f); ok {
		return n, r
	}
	cd := c.get(f)
	return cd.nslots, cd.nativeRefSlots()
}

// frameBound is function f's frame as validation bounds it
// (module.FrameBound): the numeric slots, and the reference slots its
// native code would use, of a frame that holds as much as compileFunction
// would make one hold, or more -- boundRecord's. ok is false for a body not
// validated.
func (c *Code) frameBound(f uint32) (nslots, refSlots int, ok bool) {
	nlocals, rlocals, b, ok := c.boundLocals(f)
	if !ok {
		return 0, 0, false
	}
	nslots = nlocals + 2*int(b.Stack)
	if b.Refs || rlocals > 0 || b.Tries > 0 {
		refSlots = rlocals + int(b.Stack) + int(b.Tries)
	}
	return nslots, refSlots, true
}

// withinBound reports whether function idx's frame, compiled as cd, is no
// larger than its bound (frameBound), as native code calling it sizes it:
// one that is larger is never given native code, and native calls of it
// go through Go, which sizes its frame by cd.
func (c *Code) withinBound(idx uint32, cd *compiled) bool {
	n, r, ok := c.frameBound(idx)
	return !ok || cd.nslots <= n && cd.nativeRefSlots() <= r
}

// nativeRefSlots is the number of reference slots of the function's frame
// its native code uses: none where it holds no reference.
func (cd *compiled) nativeRefSlots() int {
	if !cd.refs {
		return 0
	}
	return cd.rslots
}

// runNative runs function fn in frame l and r natively, if it has native
// code or is now compiled, returning the signal its body would: 0, or
// sigTail with the exec's tail call set.
func (x *exec) runNative(fn *Function, code *compiled, l []uint64, r []Ref) (int32, bool) {
	cj := fn.Inst.Code.jit.Load()
	if cj == nil || x.nearDepthLimit(x.depth) {
		return 0, false
	}
	nat := cj.native(fn.Inst.Code, fn.Index, code)
	if nat == nil {
		return 0, false
	}
	return x.enterNative(cj, fn, nat, l, r, nat.goEntry)
}

// nearDepthLimit reports whether a frame at call depth d is too near the
// exec's limit for native code: its calls wjit.InlineDepth deep would pass
// it. The optimizing tier's code makes the calls it inlines without
// checking the depth, so a frame runs natively only where they are within
// the limit, and the interpreter runs it otherwise, trapping where a call
// passes it.
func (x *exec) nearDepthLimit(d int) bool { return d+wjit.InlineDepth > x.maxDepth }

// osrBudget is the number of back edges a call of the function whose body
// is cd runs in the interpreter before it goes on in native code: none if
// the function has native code already, never if the JIT is off or has
// refused it.
func osrBudget(c *Code, cd *compiled) int32 {
	cj := c.jit.Load()
	switch {
	case cj == nil || cd.refused.Load():
		return math.MaxInt32
	case cd.native.Load() != nil:
		return 0
	}
	return cj.osrThreshold.Load()
}

// osr has the call of function idx running in the interpreter in frame f
// go on in the function's native code, compiling it if it has none, from
// the head of its loop at offset at: f is as the loop's head has it, as
// the native code's entry there wants it. The native code runs the call to
// its return, leaving the results where the interpreter's return does, and
// osr returns the signal for the loop to return: sigReturn, or sigTail with
// the exec's tail call set. It reports false where the function has no
// native code, or the loop no entry, and the interpreter goes on; where
// its compile found no room in the Code's JIT budget, the frame is given
// the OSR threshold's back edges, or DefaultOSRThreshold's, to run before
// it asks again.
func (x *exec) osr(f *frame, idx, at uint32) (int32, bool) {
	code := f.inst.Code
	cj := code.jit.Load()
	if cj == nil || x.nearDepthLimit(x.depth) {
		return 0, false
	}
	cd := code.get(idx)
	nat := cd.native.Load()
	if nat == nil {
		if cd.refused.Load() {
			return 0, false
		}
		if nat = cj.compile(code, idx, cd); nat == nil {
			if cj.waitsForRoom(cd) {
				f.osr = max(cj.osrThreshold.Load(), DefaultOSRThreshold)
			}
			return 0, false
		}
	} else if atomic.LoadUintptr(&cj.links[idx]) == 0 {
		cj.relink(idx, cd)
	}
	if nat.opt && nat.prev == nil {
		// The optimized code has no loop entries: the baseline's has.
		if nat = cj.loopsFor(code, idx, cd); nat == nil {
			return 0, false
		}
	}
	nat = nat.loopCode()
	entry, ok := nat.loops[at]
	if !ok {
		return 0, false
	}
	jitStats.osr.Add(1)
	sig, ok := x.enterNative(cj, f.inst.Funcs[idx], nat, f.l, f.r, entry)
	if !ok {
		jitStats.osr.Add(-1)
		return 0, false
	}
	if sig == sigTail {
		return sigTail, true
	}
	return sigReturn, true
}

// globalsAddr returns the address of the table of the instance's globals'
// addresses, which native code reads them through: a number's, or a
// reference's Ref.
func (inst *Instance) globalsAddr() uintptr {
	if len(inst.Globals) == 0 {
		return 0
	}
	return addrTable(&inst.jit.globals, len(inst.Globals), func(i int) uintptr {
		if gl := inst.Globals[i]; gl.Type.IsRef() {
			return uintptr(unsafe.Pointer(&gl.Ref))
		}
		return uintptr(unsafe.Pointer(&inst.Globals[i].Num))
	})
}

// funcsAddr and tablesAddr return the addresses of the tables of the
// instance's functions' and tables' addresses, which native code reads
// them through.
func (inst *Instance) funcsAddr() uintptr {
	if len(inst.Funcs) == 0 {
		return 0
	}
	return addrTable(&inst.jit.funcs, len(inst.Funcs), func(i int) uintptr { return uintptr(unsafe.Pointer(inst.Funcs[i])) })
}

func (inst *Instance) tablesAddr() uintptr {
	if len(inst.Tables) == 0 {
		return 0
	}
	return addrTable(&inst.jit.tables, len(inst.Tables), func(i int) uintptr { return uintptr(unsafe.Pointer(inst.Tables[i])) })
}

// addrTable returns the address of the first of the n addresses held in p,
// made of addr the first time. What they address the instance keeps.
func addrTable(p *atomic.Pointer[[]uintptr], n int, addr func(int) uintptr) uintptr {
	t := p.Load()
	if t == nil {
		s := make([]uintptr, n)
		for i := range s {
			s[i] = addr(i)
		}
		p.CompareAndSwap(nil, &s)
		t = p.Load()
	}
	return uintptr(unsafe.Pointer(&(*t)[0]))
}

const rsEntryBytes = wjit.RSEntryWords * 8

// rsStart is the address of the return stack's first word.
func (x *exec) rsStart() uintptr { return uintptr(unsafe.Pointer(&x.jit.rs[0])) }

// rsIndex is the index in the return stack of the word at addr.
func (x *exec) rsIndex(addr uintptr) int { return int((addr - x.rsStart()) / 8) }

// hasRoom reports whether the return stack has room for n more entries
// past from, the active level's.
func (x *exec) hasRoom(from uintptr, n int) bool {
	return x.jit.rs != nil && x.rsStart()+uintptr(len(x.jit.rs))*8-from >= uintptr(n*rsEntryBytes)
}

// jitRoom makes room for n more entries past the active level's, growing
// the return stack -- and moving every level's pointers into it -- if it
// has none. The exec's lock (jitExec.mu) is held where it may move it.
func (x *exec) jitRoom(from uintptr, n int) uintptr {
	if x.hasRoom(from, n) {
		return from
	}
	if x.jit.rs == nil {
		x.jit.rs = make([]uint64, 1024*wjit.RSEntryWords)
		return x.rsStart()
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

// enterNative runs fn's native code in frame l and r, as a new level, from
// offset entry -- its Go entry, or a loop's -- and handles its exits until
// the frame returns. It reports false, having run nothing, where the code
// was dropped since the caller found it (jit_mem.go).
func (x *exec) enterNative(cj *codeJIT, fn *Function, nat *nativeFn, l []uint64, r []Ref, entry int) (int32, bool) {
	cj.run.RLock()
	if nat.place.dropped.Load() {
		cj.run.RUnlock()
		return 0, false
	}
	n := x.jit.n
	if n == len(x.jit.levels) {
		x.jit.levels = append(x.jit.levels, new(jitLevel))
	}
	var from uintptr
	if n > 0 {
		from = x.jit.levels[n-1].ctx.RSP
	} else if x.jit.rs != nil {
		from = x.rsStart()
	}
	if !x.hasRoom(from, 2) {
		x.jit.mu.Lock()
		from = x.jitRoom(from, 2)
		x.jit.mu.Unlock()
	}
	lv := x.jit.levels[n]
	ch := x.jit.last
	if ch == nil || ch.id != cj.id {
		ch = x.chainOf(cj)
	}
	x.pushLevel(lv, cj, ch)
	inst := fn.Inst
	lv.inst = inst
	chunk := x.nchunks[x.nc]
	lv.nc = x.nc
	lv.chunkBase = uintptr(unsafe.Pointer(&chunk[0]))
	// The frame's reference slots are in the exec's current chunk, which
	// the frames native code makes for its calls go on in.
	rchunk := x.rchunks[x.rc]
	lv.rc = x.rc
	lv.rchunkBase = uintptr(unsafe.Pointer(&rchunk[0]))
	lv.depth, lv.calls, lv.nsp, lv.rsp = x.depth, len(x.calls), x.nsp, x.rsp
	ctx := &lv.ctx
	*ctx = wjit.Ctx{
		L:         uintptr(unsafe.Pointer(&l[0])),
		Links:     uintptr(unsafe.Pointer(&cj.links[0])),
		Globals:   inst.globalsAddr(),
		Funcs:     inst.funcsAddr(),
		Tables:    inst.tablesAddr(),
		ChunkEnd:  lv.chunkBase + uintptr(len(chunk))*8,
		R:         uintptr(unsafe.Pointer(&r[0])),
		RChunkEnd: lv.rchunkBase + uintptr(len(rchunk))*16,
		Inst:      uintptr(unsafe.Pointer(inst)),
		Frames:    uintptr(unsafe.Pointer(&cj.frames[0])),
		RFrames:   uintptr(unsafe.Pointer(&cj.rframes[0])),
	}
	lv.r0 = ctx.R
	if ctx.R < lv.rchunkBase || ctx.R >= ctx.RChunkEnd {
		cj.run.RUnlock()
		panic("wasm jit: a frame's references outside the exec's chunk")
	}
	// A native call at entry j of the level is at depth x.depth+j: the
	// entries end where the callee's calls wjit.InlineDepth deep would
	// pass the exec's maxDepth, and a call there goes through Go, which
	// traps where the call itself passes it and runs it in the interpreter
	// where its calls may (nearDepthLimit).
	lv.rsBase = from
	lv.rsDepth = uintptr(max(x.maxDepth-x.depth+1-wjit.InlineDepth, 1)) * rsEntryBytes
	ctx.RSEnd = min(x.rsStart()+uintptr(len(x.jit.rs))*8, from+lv.rsDepth)
	i := x.rsIndex(from)
	x.jit.rs[i] = uint64(nat.retStub)
	x.jit.rs[i+1] = 0
	x.jit.rs[i+2] = wjit.Site(fn.Index, 0)
	ctx.RSP = from + rsEntryBytes

	// The run lock is held for reading from here to each exit, and again
	// from the exit's work to the code's resumption, and the level ends
	// under it.
	off := entry
	for {
		if len(inst.Memories) > 0 {
			m := inst.Memories[0]
			ctx.MemBase = uintptr(m.Base())
			ctx.MemSize = m.Size()
			ctx.MemLimit = int64(ctx.MemSize) - wjit.MemLimitSlack
			ctx.MemSizePtr = uintptr(unsafe.Pointer(&m.size))
			if len(inst.Memories) > 1 {
				ctx.Mems = lv.memTable(inst.Memories)
			}
		}
		ctx.Poll = int64(x.poll)
		nat.raw.EnterAt(off, unsafe.Pointer(ctx))
		if ctx.Exit == wjit.ExitReturn {
			x.popLevel(lv)
			cj.run.RUnlock()
			x.poll = int32(ctx.Poll)
			return 0, true
		}
		cj.run.RUnlock()
		x.poll = int32(ctx.Poll)
		if t := cj.test; t != nil && t.counts != nil {
			t.counts.exits.Add(1)
			switch ctx.Exit {
			case wjit.ExitAlloc:
				t.counts.allocs.Add(1)
			case wjit.ExitOp:
				t.counts.ops.Add(1)
			case wjit.ExitInstr:
				t.counts.instrs.Add(1)
			}
		}
		var next *nativeFn
		var o int
		var tail bool
		if cj.catches.Load() {
			next, o, tail = x.jitExitCatching(lv)
		} else {
			next, o, tail = x.jitExit(lv)
		}
		if tail {
			return sigTail, true
		}
		if next != nil {
			// The exit's work took the run lock, and found next not
			// dropped.
			nat, off = next, o
			continue
		}
		// The frame that exited goes on in the code it is in, which no pass
		// drops while the level is in its exit.
		cj.run.RLock()
		nat = inst.Code.get(uint32(ctx.ExitFn)).native.Load().holding(ctx.ExitAddr)
		off = int(ctx.ExitAddr - nat.base)
	}
}

// jitExit does the work native code exited for, in level lv: it returns
// the code to enter and where, the run lock held for reading, or tail for a
// tail call the interpreter makes in place of the frame Go entered, or
// neither for the code to resume where it exited.
func (x *exec) jitExit(lv *jitLevel) (next *nativeFn, off int, tail bool) {
	ctx := &lv.ctx
	inst := lv.inst
	lv.rargs, ctx.RArgs = ctx.RArgs, 0
	switch ctx.Exit {
	case wjit.ExitCall, wjit.ExitCallIndirect, wjit.ExitCallRef, wjit.ExitOp, wjit.ExitInstr:
		lv.cj.sample(inst.Code, uint32(ctx.ExitFn), 1)
	}
	switch ctx.Exit {
	case wjit.ExitTrap:
		x.jitSync(lv, uint32(ctx.Arg1))
		trap(TrapCode(ctx.Arg0))
	case wjit.ExitPoll:
		x.pollContext()
		lv.cj.sample(inst.Code, uint32(ctx.ExitFn), optPollWeight)
	case wjit.ExitTierUp:
		fn := uint32(ctx.ExitFn)
		if nat := lv.cj.tierUp(inst.Code, fn, inst.Code.get(fn)); nat != nil {
			// The optimized code makes the call, from its start, unless
			// it was dropped since.
			lv.cj.run.RLock()
			if !nat.place.dropped.Load() {
				return nat, nat.goEntry, false
			}
			lv.cj.run.RUnlock()
		}
	case wjit.ExitCall:
		callee := inst.Funcs[ctx.Arg0]
		f := x.exitFrame(lv)
		x.jitSync(lv, uint32(ctx.Arg2))
		x.jitCall(lv, callee, callee.Sig, &f, int(ctx.Arg1))
		x.jitUnsync(lv)
	case wjit.ExitCallIndirect, wjit.ExitCallRef:
		f := x.exitFrame(lv)
		ft, slot, at := inst.Module.Types[ctx.Arg0].Func, int(ctx.Arg2), uint32(ctx.Arg3)
		if ctx.Exit == wjit.ExitCallRef {
			slot, at = int(ctx.Arg1), uint32(ctx.Arg2)
		}
		x.jitSync(lv, at)
		callee := x.jitCallee(lv, &f, ft, slot, lv.cj.inline.leaf(at))
		x.jitCall(lv, callee, ft, &f, slot)
		x.jitUnsync(lv)
	case wjit.ExitTailCall, wjit.ExitTailIndirect, wjit.ExitTailRef:
		var callee *Function
		var ft *module.FuncType
		var slot int
		var at uint32
		f := x.exitFrame(lv)
		if ctx.Exit == wjit.ExitTailCall {
			callee, slot, at = inst.Funcs[ctx.Arg0], int(ctx.Arg1), uint32(ctx.Arg2)
			ft = callee.Sig
		} else {
			ft, slot, at = inst.Module.Types[ctx.Arg0].Func, int(ctx.Arg2), uint32(ctx.Arg3)
			if ctx.Exit == wjit.ExitTailRef {
				slot, at = int(ctx.Arg1), uint32(ctx.Arg2)
			}
			x.jitSync(lv, at)
			callee = x.jitCallee(lv, &f, ft, slot, lv.cj.inline.leaf(at))
			x.jitUnsync(lv)
		}
		if n, o, done := x.jitTail(lv, callee, ft, &f, slot, at); done {
			return nil, 0, true
		} else if n != nil {
			return n, o, false
		}
	case wjit.ExitAlloc:
		f := x.exitFrame(lv)
		x.jitSync(lv, uint32(ctx.Arg1))
		x.takeFrom(lv, &f)
		x.jitAlloc(lv, ctx.Arg0, ctx.Arg2, &f, lv.cj.inline.leaf(uint32(ctx.Arg1)))
		x.jitUnsync(lv)
	case wjit.ExitInstr:
		x.jitInstr(lv)
	case wjit.ExitOp:
		f := x.exitFrame(lv)
		x.jitSync(lv, uint32(ctx.Arg4))
		x.takeFrom(lv, &f)
		x.jitOp(inst, opcode.Opcode(ctx.Arg0), uint32(ctx.Arg1), ctx.Arg2, &f, int(ctx.Arg3), lv.cj.inline.leaf(uint32(ctx.Arg4)))
		x.jitUnsync(lv)
	default:
		panic(fmt.Sprintf("wasm jit: exit %d", ctx.Exit))
	}
	return nil, 0, false
}

// memTable fills the level's table of memories for native code (Ctx.Mems)
// and returns its address: each memory's base, size and the address of its
// size, which a shared memory's grows store and other threads' too.
func (lv *jitLevel) memTable(mems []*Memory) uintptr {
	n := len(mems) * wjit.MemEntryWords
	if cap(lv.mems) < n {
		lv.mems = make([]uint64, n)
	}
	t := lv.mems[:n]
	for i, m := range mems {
		e := t[i*wjit.MemEntryWords : (i+1)*wjit.MemEntryWords]
		e[0] = uint64(uintptr(m.Base()))
		e[1] = m.Size()
		e[2] = uint64(uintptr(unsafe.Pointer(&m.size)))
	}
	return uintptr(unsafe.Pointer(&t[0]))
}

// exitFrame is the frame that exited: its slots and reference slots, and
// the positions in the chunks past them, where frames made for its calls
// go. A function that holds no reference has no reference slots of its
// own.
type exitFrame struct {
	cd        *compiled
	l         []uint64
	r         []Ref
	top, rtop int
	// rargs is the exit's Ctx.RArgs: where it is not 0, the references of
	// its operands are in other slots than their positions' (rslot).
	rargs uint64
}

func (x *exec) exitFrame(lv *jitLevel) exitFrame {
	cd := lv.inst.Code.get(uint32(lv.ctx.ExitFn))
	i := int((lv.ctx.L - lv.chunkBase) / 8)
	j := int((lv.ctx.R - lv.rchunkBase) / 16)
	n := cd.nativeRefSlots()
	return exitFrame{
		cd:    cd,
		l:     x.nchunks[lv.nc][i : i+cd.nslots : i+cd.nslots],
		r:     x.rchunks[lv.rc][j : j+n : j+n],
		top:   i + cd.nslots,
		rtop:  j + n,
		rargs: lv.rargs,
	}
}

// rslot is the reference slot of the stack position whose numeric slot is
// slot, or, for an exit that says where its references are (Ctx.RArgs),
// of the operand whose numeric slot is slot.
func (f *exitFrame) rslot(slot int) int {
	if f.rargs != 0 {
		return int(int64(f.rargs)-wjit.RArgsBias) + slot/2
	}
	return f.cd.rlocals + (slot-f.cd.nlocals)/2
}

// get returns the value of type t at the stack position whose numeric
// slot is slot: a v128's high half in the next slot.
func (f *exitFrame) get(t module.ValueType, slot int) Val {
	if t.IsRef() {
		return Val{R: f.r[f.rslot(slot)]}
	}
	if t.Kind == module.KindV128 {
		return Val{N: f.l[slot], Hi: f.l[slot+1]}
	}
	return Val{N: f.l[slot]}
}

// put stores v, of type t, at the stack position whose numeric slot is
// slot.
func (f *exitFrame) put(t module.ValueType, slot int, v Val) {
	if t.IsRef() {
		f.r[f.rslot(slot)] = v.R
		return
	}
	f.l[slot] = narrow(v.N, t)
	if t.Kind == module.KindV128 {
		f.l[slot+1] = v.Hi
	}
}

// takeFrom has frames the exec takes for Go's work go past the frame that
// exited.
func (x *exec) takeFrom(lv *jitLevel, f *exitFrame) {
	x.nc, x.nsp, x.rc, x.rsp = lv.nc, f.top, lv.rc, f.rtop
}

// jitSync makes the exec's call stack and depth say what the level's
// native frames are, the innermost at offset, for a call of Go to see and
// a trap to report.
func (x *exec) jitSync(lv *jitLevel, offset uint32) {
	rs := x.jit.rs
	x.calls = x.calls[:lv.calls]
	x.jit.entries = x.rsEntries(lv, x.jit.entries)
	for k, i := range x.jit.entries {
		site := rs[i+2]
		callee := lv.inst.Funcs[site>>32]
		if k == 0 {
			x.calls[len(x.calls)-1].fn = callee
			continue
		}
		x.syncAt(lv, uint32(site))
		x.calls = append(x.calls, callSite{fn: callee})
	}
	x.syncAt(lv, offset)
	x.depth = lv.depth + len(x.calls) - lv.calls
}

// jitUnsync puts the exec's state back as the level found it.
func (x *exec) jitUnsync(lv *jitLevel) {
	x.calls = x.calls[:lv.calls]
	x.depth = lv.depth
	x.nc, x.nsp, x.rc, x.rsp = lv.nc, lv.nsp, lv.rc, lv.rsp
}

// jitArgs reads a call's arguments of signature ft from the stack
// positions from the one whose numeric slot is slot.
func (lv *jitLevel) jitArgs(ft *module.FuncType, f *exitFrame, slot int) []Val {
	n := len(ft.Params)
	if cap(lv.args) < n {
		lv.args = make([]Val, n)
	}
	args := lv.args[:n]
	for i, t := range ft.Params {
		args[i] = f.get(t, slot+2*i)
	}
	return args
}

// jitCallee returns the function a call_indirect or a call_ref of type ft
// calls, its arguments from the stack position whose numeric slot is slot
// and the table's index or the reference after them, trapping as the
// interpreter does, as the instruction at offset at.
func (x *exec) jitCallee(lv *jitLevel, f *exitFrame, ft *module.FuncType, slot int, at uint32) *Function {
	ctx := &lv.ctx
	k := slot + 2*len(ft.Params)
	if ctx.Exit == wjit.ExitCallRef || ctx.Exit == wjit.ExitTailRef {
		r := f.r[f.rslot(k)]
		if r.IsNull() {
			trapAt(TrapNullDereference, uint64(at))
		}
		return r.Func()
	}
	return indirectTarget(lv.inst, uint32(ctx.Arg0), uint32(ctx.Arg1), f.l[k], at)
}

// jitCall calls fn, of signature ft, for native code: its arguments in the
// stack positions from the one whose numeric slot is slot, where its
// results go. The frames it makes go past the frame that exited.
func (x *exec) jitCall(lv *jitLevel, fn *Function, ft *module.FuncType, f *exitFrame, slot int) {
	args := lv.jitArgs(ft, f, slot)
	m := len(ft.Results)
	if cap(lv.res) < m {
		lv.res = make([]Val, m)
	}
	res := lv.res[:m]
	x.takeFrom(lv, f)
	// A native call finding the return stack full came here: make it room.
	if !x.hasRoom(lv.ctx.RSP, 64) {
		x.jit.mu.Lock()
		x.jitRoom(lv.ctx.RSP, 64)
		x.jit.mu.Unlock()
	}
	x.callGeneric(fn, args, res)
	for i, t := range ft.Results {
		f.put(t, slot+2*i, res[i])
	}
	clear(res)
	clear(args)
}

// jitTail makes a tail call of fn for the frame that exited: natively in
// its place where fn has native code and room, returning the code to enter,
// the run lock held for reading; as the interpreter's tail call (done) if
// the frame is the one Go entered; else by calling fn and returning its
// results, the frame's code resumed at its return.
func (x *exec) jitTail(lv *jitLevel, fn *Function, ft *module.FuncType, f *exitFrame, slot int, at uint32) (*nativeFn, int, bool) {
	ctx := &lv.ctx
	args := lv.jitArgs(ft, f, slot)
	defer clear(args)
	// A tail call is a call: it counts toward the poll, as the
	// interpreter's do.
	if x.poll--; x.poll <= 0 {
		x.pollContext()
	}
	// The frame's depth: the level's, and an entry each for the frames
	// above the one Go entered.
	depth := x.depth + int((ctx.RSP-lv.rsBase)/rsEntryBytes) - 1
	if fn.Inst == lv.inst && fn.Host == nil && !x.nearDepthLimit(depth) {
		cd := lv.inst.Code.get(fn.Index)
		start, rstart := f.top-len(f.l), f.rtop-len(f.r)
		nchunk, rchunk := x.nchunks[lv.nc], x.rchunks[lv.rc]
		if nat := lv.cj.native(lv.inst.Code, fn.Index, cd); nat != nil && start+cd.nslots <= len(nchunk) && rstart+cd.nativeRefSlots() <= len(rchunk) {
			lv.cj.run.RLock()
			if !nat.place.dropped.Load() {
				n := cd.nativeRefSlots()
				putVals(nchunk[start:start+cd.nslots], rchunk[rstart:rstart+n], cd.params, args)
				e := x.rsIndex(ctx.RSP) - wjit.RSEntryWords
				x.jit.rs[e+2] = wjit.Site(fn.Index, uint32(x.jit.rs[e+2]))
				return nat, nat.goEntry, false
			}
			lv.cj.run.RUnlock()
		}
	}
	if ctx.RSP-lv.rsBase == rsEntryBytes {
		// The frame Go entered: the interpreter's tail call makes it.
		x.tail = fn
		x.tailArgs = append(x.tailArgs[:0], args...)
		lv.cj.run.RLock()
		x.popLevel(lv)
		lv.cj.run.RUnlock()
		return nil, 0, true
	}
	x.jitSync(lv, at)
	m := len(ft.Results)
	if cap(lv.res) < m {
		lv.res = make([]Val, m)
	}
	res := lv.res[:m]
	x.takeFrom(lv, f)
	x.callGeneric(fn, args, res)
	results, _, _ := layout(ft.Results)
	for i, t := range ft.Results {
		if s := results[i]; s.ref {
			f.r[s.i] = res[i].R
		} else {
			f.l[s.i] = narrow(res[i].N, t)
			if s.v128 {
				f.l[s.i+1] = res[i].Hi
			}
		}
	}
	clear(res)
	x.jitUnsync(lv)
	return nil, 0, false
}

// indirectTarget returns the function call_indirect of type typeIndex
// through table calls with index i, trapping as the interpreter does, as
// the instruction at offset at.
func indirectTarget(inst *Instance, typeIndex, table uint32, i uint64, at uint32) *Function {
	if !inst.Module.Tables[table].Limits.Is64 {
		i = uint64(uint32(i))
	}
	return indirectCallee(inst, typeIndex, table, i, indirectChecksType(inst.Module, typeIndex, table), uint64(at))
}

// jitOp runs instruction op for native code, with immediates a and imm, its
// operands in the stack positions of the exited frame from the one whose
// numeric slot is slot, its result left in the first. imm is an atomic
// instruction's offset, 64 bits; any other instruction's second immediate
// is 32. at is the instruction's offset in the module, for its traps.
func (x *exec) jitOp(inst *Instance, op opcode.Opcode, a uint32, imm uint64, f *exitFrame, slot int, at uint32) {
	b := uint32(imm)
	m := inst.Module
	frame := f.l
	arg := func(i int, is64 bool) uint64 {
		if is64 {
			return frame[slot+2*i]
		}
		return uint64(uint32(frame[slot+2*i]))
	}
	ref := func(i int) Ref { return f.r[f.rslot(slot+2*i)] }
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
		checkRange(src, cnt, uint64(len(data)), TrapMemOutOfBounds, uint64(at))
		checkRange(dst, cnt, uint64(len(buf)), TrapMemOutOfBounds, uint64(at))
		copy(buf[dst:dst+cnt], data[src:src+cnt])
	case opcode.DataDrop:
		inst.datas[a] = nil
	case opcode.MemoryCopy:
		d64, s64 := mem64(a), mem64(b)
		dst, src, cnt := arg(0, d64), arg(1, s64), arg(2, d64 && s64)
		db, sb := inst.Memories[a].bytes(), inst.Memories[b].bytes()
		checkRange(src, cnt, uint64(len(sb)), TrapMemOutOfBounds, uint64(at))
		checkRange(dst, cnt, uint64(len(db)), TrapMemOutOfBounds, uint64(at))
		copy(db[dst:dst+cnt], sb[src:src+cnt])
	case opcode.MemoryFill:
		is64 := mem64(a)
		dst, val, cnt := arg(0, is64), byte(arg(1, false)), arg(2, is64)
		buf := inst.Memories[a].bytes()
		checkRange(dst, cnt, uint64(len(buf)), TrapMemOutOfBounds, uint64(at))
		s := buf[dst : dst+cnt]
		for i := range s {
			s[i] = val
		}
	case opcode.TableInit:
		// a is the segment, b the table.
		dst, src, cnt := arg(0, tab64(b)), arg(1, false), arg(2, false)
		elems := inst.elems[a]
		t := inst.Tables[b]
		checkRange(dst, cnt, uint64(len(t.Elems)), TrapTableOutOfBounds, uint64(at))
		checkRange(src, cnt, uint64(len(elems)), TrapElementSegmentOutOfBounds, uint64(at))
		copy(t.Elems[dst:dst+cnt], elems[src:src+cnt])
	case opcode.ElemDrop:
		inst.dropElem(a)
	case opcode.TableCopy:
		d64, s64 := tab64(a), tab64(b)
		dst, src, cnt := arg(0, d64), arg(1, s64), arg(2, d64 && s64)
		de, se := inst.Tables[a].Elems, inst.Tables[b].Elems
		checkRange(src, cnt, uint64(len(se)), TrapTableOutOfBounds, uint64(at))
		checkRange(dst, cnt, uint64(len(de)), TrapTableOutOfBounds, uint64(at))
		copy(de[dst:dst+cnt], se[src:src+cnt])
	case opcode.TableSize:
		put(inst.Tables[a].Size(), tab64(a))
	case opcode.AtomicNotify:
		// a is the memory, imm the offset.
		mm := inst.Memories[a]
		ea := atomicEA(arg(0, mem64(a)), imm, mem64(a), at)
		cnt := uint32(frame[slot+2])
		atomicSpan(mm, ea, 4, uint64(at))
		put(uint64(notify(mm, ea, cnt)), false)
	case opcode.I32AtomicWait, opcode.I64AtomicWait:
		width, want := uint64(4), uint64(uint32(frame[slot+2]))
		if op == opcode.I64AtomicWait {
			width, want = 8, frame[slot+2]
		}
		mm := inst.Memories[a]
		ea := atomicEA(arg(0, mem64(a)), imm, mem64(a), at)
		timeout := int64(frame[slot+4])
		if !mm.Shared {
			trapAt(TrapAtomicsWaitNotAllowed, uint64(at))
		}
		put(uint64(wait(x, mm, atomicSpan(mm, ea, width, uint64(at)), ea, width, want, timeout)), false)
	case opcode.TableGrow:
		init := ref(0)
		put(uint64(inst.Tables[a].Grow(arg(1, tab64(a)), init)), tab64(a))
	case opcode.Throw:
		params := m.TagType(a).Params
		vals := make([]Val, len(params))
		for i, t := range params {
			vals[i] = f.get(t, slot+2*i)
		}
		panic(&Exception{hdr: header{kind: ObjExn}, Tag: inst.Tags[a], Vals: vals})
	case opcode.Rethrow:
		// a is the reference slot of the exception a legacy catch caught.
		panic(f.r[a].Exn())
	case opcode.ThrowRef:
		r := ref(0)
		if r.IsNull() {
			trapAt(TrapRethrowNull, uint64(at))
		}
		panic(r.Exn())
	case opcode.TableFill:
		is64 := tab64(a)
		dst, val, cnt := arg(0, is64), ref(1), arg(2, is64)
		e := inst.Tables[a].Elems
		checkRange(dst, cnt, uint64(len(e)), TrapTableOutOfBounds, uint64(at))
		s := e[dst : dst+cnt]
		for i := range s {
			s[i] = val
		}
	default:
		if !x.jitGCOp(inst, op, a, b, f, slot, at) {
			panic("wasm jit: no Go operation " + op.Name())
		}
	}
}

// atomicEA is the effective address of an access at addr plus offset off
// by the instruction at offset at: an i64 memory's traps where the sum
// carries, as the interpreter's addr does.
func atomicEA(addr, off uint64, is64 bool, at uint32) uint64 {
	ea := addr + off
	if is64 && ea < addr {
		trapAt(TrapMemOutOfBounds, uint64(at))
	}
	return ea
}
