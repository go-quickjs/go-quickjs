//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"math"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"weak"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
	wopt "github.com/go-quickjs/go-quickjs/wasm/internal/jit/opt"
)

// The optimizing tier's compiles in the background
// (wasm/internal/jit/README.md, Tiers). A function whose baseline code
// counted enough calls is queued for the process's workers, its counter
// set so that its baseline code exits no more, and the call that asked
// goes on in the baseline code, as later calls do until the optimized code
// is published. A worker compiles it from what does not change while code
// runs -- the module, the function's body and its record's frame -- and
// places the code apart from the arena's code that may be running
// (ijit.PlaceRawApart), without the Code's run lock; publishing it takes
// the Code's compile lock for a moment, the one a first-call compile
// takes, to store the function's native code and its link.

// optBackground is a Code's state of its compiles in the background.
type optBackground struct {
	// mode says whether tier-ups are compiled in the background: optAuto
	// where GOMAXPROCS is above 1, optOn always, optOff never
	// (SetOptBackground).
	mode atomic.Int32
	// owner is the Code, which a queued request does not keep alive and a
	// worker holds while it compiles one of its functions: the Code's
	// cleanup, which gives the arena back, cannot run meanwhile.
	owner weak.Pointer[Code]
	// queue holds the functions waiting for a worker, and queued says the
	// code is in the pool's ring; both are the pool's (optPool.mu).
	queue  []uint32
	queued bool
	// pending counts the functions queued, being compiled or waiting to
	// be placed.
	pending atomic.Int32
	// ready holds the compiles finished and not yet placed (readyMu);
	// placing serializes their placements, each taking all that are ready.
	readyMu sync.Mutex
	ready   []optReady
	placing sync.Mutex
	// closed says the code was given back, and nothing more is published
	// (codeJIT.mu).
	closed bool
}

// optReady is a compile finished in the background: function idx's
// optimized code, not yet placed.
type optReady struct {
	idx  uint32
	cd   *compiled
	code *wjit.Code
}

// The modes of the compiles in the background (optBackground.mode).
const (
	optAuto int32 = iota
	optOn
	optOff
)

// optBackgroundEnv is the mode WASM_JIT_OPT_BACKGROUND sets: optOn for "1"
// or "on", optOff for "0" or "off", -1 for none -- the mode then optAuto,
// but optOff where WASM_JIT compiles every function at its first call and
// wants the tiering the same on every run ("eager", "osr", "opt").
var optBackgroundEnv = func() int32 {
	switch os.Getenv("WASM_JIT_OPT_BACKGROUND") {
	case "1", "on":
		return optOn
	case "0", "off":
		return optOff
	}
	return -1
}()

// optBackgroundDefault is the mode of a Code made now.
func optBackgroundDefault() int32 {
	if optBackgroundEnv >= 0 {
		return optBackgroundEnv
	}
	if jitEnvOSR != 0 || os.Getenv("WASM_JIT") == "opt" {
		return optOff
	}
	return optAuto
}

func (c *Code) setOptBackground(on bool) {
	if cj := c.jit.Load(); cj != nil {
		mode := optOff
		if on {
			mode = optOn
		}
		cj.bg.mode.Store(mode)
	}
}

// inBackground reports whether the code's tier-ups are compiled in the
// background now: in the mode optAuto, where GOMAXPROCS is above 1 -- with
// one, the worker would run only when the program's goroutine is
// preempted, and the optimized code come later than compiled on the spot
// -- and never while a host times the compiles (wopt.Timing), which it does
// on one goroutine.
func (cj *codeJIT) inBackground() bool {
	switch cj.bg.mode.Load() {
	case optOff:
		return false
	case optAuto:
		if runtime.GOMAXPROCS(0) == 1 {
			return false
		}
	}
	return wopt.Timing == nil
}

// tierUpLater queues function idx, whose baseline code counted enough
// calls, for a worker, once: its counter is set so that its baseline code
// exits no more meanwhile, and stored again by every later ask, which a
// call's decrement racing with the store may have undone.
func (cj *codeJIT) tierUpLater(idx uint32, cd *compiled) {
	atomic.StoreInt64(&cj.counters[idx], math.MaxInt64)
	if cd.optDone.CompareAndSwap(false, true) {
		optWorkers.submit(cj, idx)
	}
}

// optWorkers are the process's workers compiling the optimizing tier's
// code, shared by every Code: at most a quarter of GOMAXPROCS, at least
// one, each started when a request is queued and none is free, each ending
// once none is queued, so that an idle process runs none.
var optWorkers optPool

type optPool struct {
	mu sync.Mutex
	// ring holds the codes with requests queued, served in turn, so that
	// one code's many requests do not hold up another's.
	ring    []*codeJIT
	running int
}

// optWorkerLimit is how many workers compile at once.
func optWorkerLimit() int { return max(1, runtime.GOMAXPROCS(0)/4) }

// submit queues function idx of cj, starting a worker if fewer than the
// limit run.
func (p *optPool) submit(cj *codeJIT, idx uint32) {
	cj.bg.pending.Add(1)
	p.mu.Lock()
	cj.bg.queue = append(cj.bg.queue, idx)
	if !cj.bg.queued {
		cj.bg.queued = true
		p.ring = append(p.ring, cj)
	}
	start := p.running < optWorkerLimit()
	if start {
		p.running++
	}
	p.mu.Unlock()
	if start {
		go p.work()
	}
}

// next takes the request to compile next, p.mu held: of the ring's first
// code, the function its baseline code was called most often since it was
// queued, which its counter, set to math.MaxInt64 then, counts down -- a
// function no longer called waits behind those that are -- and the code
// goes to the ring's end if it has more.
func (p *optPool) next() (*codeJIT, uint32, bool) {
	if len(p.ring) == 0 {
		return nil, 0, false
	}
	cj := p.ring[0]
	p.ring[0] = nil
	p.ring = p.ring[1:]
	q := cj.bg.queue
	best, bestCalls := 0, int64(-1)
	for i, idx := range q {
		calls := int64(math.MaxInt64)
		if n := atomic.LoadInt64(&cj.counters[idx]); n >= 0 {
			calls = math.MaxInt64 - n
		}
		if calls > bestCalls {
			best, bestCalls = i, calls
		}
	}
	idx := q[best]
	cj.bg.queue = append(q[:best], q[best+1:]...)
	if len(cj.bg.queue) > 0 {
		p.ring = append(p.ring, cj)
	} else {
		cj.bg.queue, cj.bg.queued = nil, false
	}
	return cj, idx, true
}

// work compiles the queued requests until none is left.
func (p *optPool) work() {
	for {
		p.mu.Lock()
		cj, idx, ok := p.next()
		if !ok {
			p.running--
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		cj.compileInBackground(idx)
	}
}

// drop removes cj's queued requests, once its code is given back.
func (p *optPool) drop(cj *codeJIT) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !cj.bg.queued {
		return
	}
	for i, q := range p.ring {
		if q == cj {
			p.ring = append(p.ring[:i], p.ring[i+1:]...)
			break
		}
	}
	cj.bg.pending.Add(-int32(len(cj.bg.queue)))
	cj.bg.queue, cj.bg.queued = nil, false
}

// compileInBackground compiles function idx with the optimizing tier on a
// worker, and places and publishes its code with those of the other
// compiles of the code finished meanwhile. A refusal leaves the function
// its baseline code, its counter run out for good, as a refusal on the
// spot does. A Code collected while the request was queued is not
// compiled for.
func (cj *codeJIT) compileInBackground(idx uint32) {
	if optWorkerHook != nil {
		optWorkerHook()
	}
	c := cj.bg.owner.Value()
	if c == nil {
		cj.bg.pending.Add(-1)
		return
	}
	defer runtime.KeepAlive(c)
	cd := c.get(idx)
	if !c.withinBound(idx, cd) {
		countOptRefused(wjit.RefuseFrame)
		cj.bg.pending.Add(-1)
		return
	}
	start := time.Now()
	code, err := wopt.Compile(request(c, idx, cd, 0))
	countTime(&jitStats.optTime, start)
	if err != nil {
		reason := "internal"
		if r, ok := err.(*wjit.Refusal); ok {
			reason = r.Reason
		}
		countOptRefused(reason)
		cj.bg.pending.Add(-1)
		return
	}
	cj.bg.readyMu.Lock()
	cj.bg.ready = append(cj.bg.ready, optReady{idx: idx, cd: cd, code: code})
	cj.bg.readyMu.Unlock()
	cj.placeReady()
}

// placeReady places every finished compile of the code in one batch, apart
// from the code that may be running (ijit.PlaceRawApart), and publishes
// each: the function's native code becomes the optimized code, the
// baseline code it replaces kept as its prev for the frames running in it,
// and its link the optimized code's entry. A worker whose compile another
// placed finds none ready.
func (cj *codeJIT) placeReady() {
	cj.bg.placing.Lock()
	defer cj.bg.placing.Unlock()
	cj.bg.readyMu.Lock()
	batch := cj.bg.ready
	cj.bg.ready = nil
	cj.bg.readyMu.Unlock()
	if len(batch) == 0 {
		return
	}
	defer cj.bg.pending.Add(-int32(len(batch)))
	codes := make([][]byte, len(batch))
	for i, b := range batch {
		codes[i] = b.code.Bytes
	}
	placeStart := time.Now()
	raws, err := ijit.PlaceRawApart(cj.arena, codes...)
	countTime(&jitStats.placeTime, placeStart)
	if err != nil {
		for range batch {
			countOptRefused("unplaced")
		}
		return
	}
	cj.mu.Lock()
	defer cj.mu.Unlock()
	if cj.bg.closed {
		for _, r := range raws {
			r.Close()
		}
		return
	}
	for i, b := range batch {
		cj.publishOpt(b.idx, b.cd, raws[i], b.code, b.cd.native.Load())
	}
}

// optWorkerHook, for tests, runs before each compile in the background.
var optWorkerHook func()

// waitOptBackground waits until no function of the code is queued, being
// compiled or waiting to be placed in the background, for tests.
func (c *Code) waitOptBackground() {
	cj := c.jit.Load()
	if cj == nil {
		return
	}
	for cj.bg.pending.Load() > 0 {
		time.Sleep(50 * time.Microsecond)
	}
}

// waitOptWorkers waits until the process's workers have ended, for tests.
func waitOptWorkers() {
	for {
		optWorkers.mu.Lock()
		n := optWorkers.running
		optWorkers.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
}
