//go:build quickjs_jit && !android && !ios && ((amd64 && (linux || windows)) || (arm64 && (darwin || linux)))

package engine

import (
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"
	"weak"

	ijit "github.com/go-quickjs/go-quickjs/internal/jit"
	wjit "github.com/go-quickjs/go-quickjs/wasm/internal/jit"
)

// What the JIT holds for a Code, and what it drops to stay within the
// Code's budget (JITBudget).
//
// A compile reserves room in the budget for its code and its record before
// placing it (codeJIT.reserve). Where the budget is full, a pass over the
// Code's functions (reclaim), at most one an epoch of the JIT's clock,
// makes room, then a pass over the other Codes of the budget whose compiles
// are not under way:
//
//   - Code the optimizing tier replaced, or that a loop's entry was compiled
//     for, which no frame is in, is dropped.
//   - A function whose code no frame is in has its link cleared: its next
//     call, native or not, comes through Go, which notes the call and links
//     the code again. A function whose link is still clear a pass
//     jitCodeAge epochs later was not called since, and its code is
//     dropped, the least recently called first, until the room is made. Its
//     next call compiles it again, as its first did.
//
// What is left of the room, if any, the compile does not get: the function
// runs in the interpreter, its body compiled for it then, and is compiled
// again no earlier than the next epoch.
//
// Dropping is safe because nothing can run the code or return into it
// after it is gone:
//
//   - The pass holds the Code's run lock for writing, so no thread runs the
//     Code's native code: every native frame of the Code is in a level of
//     an exec (jitLevel) that has exited to Go, and each goes on at an
//     address the level holds -- on the exec's return stack, each frame's
//     caller's return address, and the level's exit address for the frame
//     that exited. The pass reads every level of the Code's of every exec
//     (jitExecs), under the exec's lock, which the exec holds while it
//     begins a level or moves its return stack (pins). A code holding any
//     of those addresses is kept, and with it its function's current code
//     and handlers.
//   - A code is entered only through the link table, which native calls
//     read while they hold the run lock, and which the pass clears first;
//     or from Go -- a call, a loop's entry, a tier-up, a tail call, a catch
//     clause's landing pad -- which takes the run lock and checks that the
//     code was not dropped (placedCode.dropped) before it enters it, links
//     it again or changes the frames for it, and enters none where it was:
//     the call runs in the interpreter, or goes on as it would without it.
//   - Its pages go back to the arena, which reuses them for later code
//     (ijit.NewReusingArena) and unmaps a chunk once no code is in it.

// placedCode is a code placed in the Code's arena: what it holds, charged
// to the Code's budget, and whether it has been dropped. The records of one
// function's code share it.
type placedCode struct {
	raw *ijit.RawCode
	// code and record are the code's bytes in the arena and what its
	// record holds, as charged.
	code, record int64
	// dropped is set once the code is dropped, under the run lock held for
	// writing: under the read lock, a code not dropped stays until it is
	// released.
	dropped atomic.Bool
}

// The records' sizes, as charged: a record (nativeFn) and its code's
// (placedCode, ijit.RawCode); each loop entry, an entry of a map; each
// catch clause, and each try_table or try; and the code of a SIMD
// instruction native code leaves to Go, a few closures.
const (
	nativeRecordBytes = int64(unsafe.Sizeof(nativeFn{}) + unsafe.Sizeof(placedCode{}) + 3*unsafe.Sizeof(uintptr(0)))
	loopMapBytes      = 48
	loopEntryBytes    = 24
	handlerBytes      = int64(unsafe.Sizeof(wjit.Handler{}))
	clauseBytes       = int64(unsafe.Sizeof(wjit.Clause{}))
	simdExitBytes     = 256
)

// recordBytes is what the record of a code with loops' entries loops and
// catch handlers handlers holds.
func recordBytes(loops map[uint32]int, handlers []wjit.Handler) int64 {
	n := nativeRecordBytes
	if len(loops) > 0 {
		n += loopMapBytes + int64(len(loops))*loopEntryBytes
	}
	for _, h := range handlers {
		n += handlerBytes + int64(len(h.Clauses))*clauseBytes
	}
	return n
}

// tableBytes is what the tables of a Code of n functions hold: its links,
// frames, counters and functions' states, and the codeJIT itself.
func tableBytes(n int) int64 {
	return int64(unsafe.Sizeof(codeJIT{})) + int64(n)*int64(3*8+unsafe.Sizeof((*fnJIT)(nil)))
}

// setJITBudget is SetJITBudget.
func (c *Code) setJITBudget(b *JITBudget) {
	cj := c.jit.Load()
	if cj == nil || b == nil || jitMemEnv > 0 {
		return
	}
	cj.mu.Lock()
	defer cj.mu.Unlock()
	cj.joinBudget(b)
}

// joinBudget charges what the code holds to b, and no longer to the budget
// it had. cj.mu is held, or the code is new.
func (cj *codeJIT) joinBudget(b *JITBudget) {
	if old := cj.budget; old != nil {
		if old == b {
			return
		}
		old.mu.Lock()
		delete(old.members, cj)
		old.mu.Unlock()
		old.release(cj.held())
	}
	b.mu.Lock()
	b.members[cj] = struct{}{}
	b.mu.Unlock()
	b.charge(cj.held())
	cj.budget = b
}

// held is what the code holds, as charged to its budget.
func (cj *codeJIT) held() int64 { return cj.codeBytes.Load() + cj.recordBytes.Load() + cj.tableBytes }

// jitMemory is JITMemory.
func (c *Code) jitMemory() JITMemory {
	cj := c.jit.Load()
	if cj == nil {
		return JITMemory{}
	}
	mapped, _ := cj.arena.Mapped()
	cj.mu.Lock()
	n := 0
	for _, f := range cj.fns {
		if f != nil && f.native.Load() != nil {
			n++
		}
	}
	cj.mu.Unlock()
	return JITMemory{Code: cj.codeBytes.Load(), Records: cj.recordBytes.Load(), Tables: cj.tableBytes,
		Mapped: int64(mapped), Functions: n, Dropped: int(cj.dropped.Load())}
}

// leaveBudget gives back what the code held, once it is closed.
func (cj *codeJIT) leaveBudget() {
	b := cj.budget
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.members, cj)
	b.mu.Unlock()
	b.release(cj.held())
	cj.codeBytes.Store(0)
	cj.recordBytes.Store(0)
	cj.tableBytes = 0
}

// leastBytes is the least a compile of a body of size bytes needs: a byte
// of code for each byte of body, and a record.
func leastBytes(size uint32) int64 {
	return int64(alignCode(int(size))) + nativeRecordBytes
}

// mayFit reports whether a compile of function idx, whose record is cd, by
// the optimizing tier if opt says, may find room in the budget, making room
// for the least it needs if it must -- leastBytes, or what the tier's code
// needed when it last found no room -- so that a compile whose code could
// not be placed is mostly not made: where all the code is in use, or the
// budget is too small for the function. cj.mu is held.
func (cj *codeJIT) mayFit(c *Code, idx uint32, cd *compiled, opt bool) bool {
	b := cj.budget
	if b.limit <= 0 {
		return true
	}
	least := max(leastBytes(c.Module.Functions[idx].Code.Len), cd.roomNeed[tierOf(opt)])
	if b.fits(least) {
		return true
	}
	if !cj.reserve(least) {
		return false
	}
	b.release(least)
	return true
}

// tierOf is the index of a tier in fnJIT.roomNeed.
func tierOf(opt bool) int {
	if opt {
		return 1
	}
	return 0
}

// noRoom refuses a compile of function idx, whose code does not fit the
// budget, or would not, need bytes if it was made and is known: it runs in
// the interpreter, and is not compiled again before the next epoch. cj.mu
// is held.
func noRoom(cd *compiled, opt bool, need int64) {
	if need > 0 {
		cd.roomNeed[tierOf(opt)] = need
	}
	cd.noRoom.Store(jitEpoch())
	if opt {
		countOptRefused(RefuseBudget)
	} else {
		countRefused(RefuseBudget)
	}
}

// waitsForRoom reports whether function cd's compile found no room in this
// epoch.
func waitsForRoom(cd *compiled) bool {
	r := cd.noRoom.Load()
	return r != 0 && jitEpoch() <= r
}

// reserve takes need bytes of the budget for a code about to be placed,
// making room as the budget's policy says (above) if it must, and reports
// whether it has them. cj.mu is held.
func (cj *codeJIT) reserve(need int64) bool {
	b := cj.budget
	if b.reserve(need) {
		return true
	}
	if b.limit > 0 && need > b.limit {
		return false
	}
	now := jitEpoch()
	if cj.reclaimedAt != now {
		cj.reclaimedAt = now
		cj.reclaim(now, need)
		if b.reserve(need) {
			return true
		}
	}
	b.mu.Lock()
	others := make([]*codeJIT, 0, len(b.members))
	for o := range b.members {
		if o != cj {
			others = append(others, o)
		}
	}
	b.mu.Unlock()
	for _, o := range others {
		// Not one whose compile is under way: it may be making room
		// itself, in this budget's other Codes.
		if !o.mu.TryLock() {
			continue
		}
		if o.budget == b && o.reclaimedAt != now && !o.bg.closed {
			o.reclaimedAt = now
			o.reclaim(now, need)
		}
		o.mu.Unlock()
		if b.reserve(need) {
			return true
		}
	}
	return false
}

// place places code, of need bytes with its record, charged to the budget,
// which reserve has taken them from; it gives them back if the code cannot
// be placed. cj.mu is held.
func (cj *codeJIT) place(code []byte, record int64) (*placedCode, error) {
	cj.run.Lock()
	raw, err := ijit.PlaceRaw(cj.arena, code)
	cj.run.Unlock()
	if err != nil {
		return nil, err
	}
	pc := &placedCode{raw: raw, code: int64(raw.Size()), record: record}
	cj.codeBytes.Add(pc.code)
	cj.recordBytes.Add(record)
	cj.raws = append(cj.raws, pc)
	return pc, nil
}

// placeCharged reserves room for code and its record and places it,
// returning nil and the reason where it gets no room or no place. cj.mu is
// held.
func (cj *codeJIT) placeCharged(code []byte, record int64) (*placedCode, string) {
	need := placedNeed(code, record)
	if !cj.reserve(need) {
		return nil, RefuseBudget
	}
	pc, err := cj.place(code, record)
	if err != nil {
		cj.budget.release(need)
		return nil, "unplaced"
	}
	if d := pc.code + pc.record - need; d != 0 {
		// The arena rounded the code otherwise than alignCode.
		cj.budget.charge(d)
	}
	return pc, ""
}

// placedNeed is what code and its record take of the budget once placed.
func placedNeed(code []byte, record int64) int64 { return int64(alignCode(len(code))) + record }

// alignCode is the bytes the arena gives code of n bytes: a whole number of
// cache lines.
func alignCode(n int) int { return (n + 63) &^ 63 }

// drop gives code pc back to the arena and its bytes to the budget. The run
// lock is held for writing and cj.mu.
func (cj *codeJIT) drop(pc *placedCode) {
	if pc.dropped.Swap(true) {
		return
	}
	pc.raw.Close()
	cj.codeBytes.Add(-pc.code)
	cj.recordBytes.Add(-pc.record)
	cj.budget.release(pc.code + pc.record)
	if i := slices.Index(cj.raws, pc); i >= 0 {
		cj.raws[i] = cj.raws[len(cj.raws)-1]
		cj.raws[len(cj.raws)-1] = nil
		cj.raws = cj.raws[:len(cj.raws)-1]
	}
}

// pins returns, sorted, the addresses native frames of the code go on at:
// each level's frames' return addresses and its exit's address, in every
// exec (jitExecs). The run lock is held for writing.
func (cj *codeJIT) pins() []uintptr {
	var pins []uintptr
	jitExecs.mu.Lock()
	for _, w := range jitExecs.list {
		x := w.Value()
		if x == nil {
			continue
		}
		x.jit.mu.Lock()
		rs := x.jit.rs
		if len(rs) > 0 {
			start := uintptr(unsafe.Pointer(&rs[0]))
			for _, lv := range x.jit.levels[:x.jit.n.Load()] {
				if lv.cj != cj {
					continue
				}
				pins = append(pins, lv.ctx.ExitAddr, lv.ctx.Resume)
				if lv.rsBase < start || lv.ctx.RSP < lv.rsBase {
					continue
				}
				first, last := int((lv.rsBase-start)/8), min(int((lv.ctx.RSP-start)/8), len(rs))
				for i := first; i < last; i += wjit.RSEntryWords {
					pins = append(pins, uintptr(rs[i]))
				}
			}
		}
		x.jit.mu.Unlock()
	}
	jitExecs.mu.Unlock()
	slices.Sort(pins)
	return pins
}

// pinned reports whether a frame goes on in n's code: whether it holds an
// address of pins.
func (n *nativeFn) pinned(pins []uintptr) bool {
	i, _ := slices.BinarySearch(pins, n.base)
	return i < len(pins) && pins[i] < n.end
}

// reclaim drops what the code holds that is safe to drop, until need more
// bytes fit the budget, as the policy above says: a pass at epoch now.
// cj.mu is held.
func (cj *codeJIT) reclaim(now uint64, need int64) {
	cj.run.Lock()
	defer cj.run.Unlock()
	pins := cj.pins()
	type old struct {
		idx  int
		used uint64
	}
	var olds []old
	for idx, f := range cj.fns {
		if f == nil {
			continue
		}
		nat := f.native.Load()
		if nat == nil {
			continue
		}
		pinned := nat.pinned(pins)
		if nat.prev != nil {
			// The codes it replaced, but those a frame is in.
			var keep []*nativeFn
			for p := nat.prev; p != nil; p = p.prev {
				if p.pinned(pins) {
					keep = append(keep, p)
					pinned = true
				} else {
					cj.drop(p.place)
					jitStats.droppedReplaced.Add(1)
				}
			}
			if len(keep) != chainLen(nat)-1 {
				f.native.Store(nat.relinked(keep))
			}
		}
		if pinned {
			f.used.Store(now)
			continue
		}
		if atomic.LoadUintptr(&cj.links[idx]) != 0 {
			// Called through Go next, which links it again.
			atomic.StoreUintptr(&cj.links[idx], 0)
			f.used.Store(now)
			continue
		}
		if u := f.used.Load(); u+jitCodeAge <= now {
			olds = append(olds, old{idx, u})
		}
	}
	slices.SortStableFunc(olds, func(a, b old) int {
		switch {
		case a.used < b.used:
			return -1
		case a.used > b.used:
			return 1
		}
		return 0
	})
	for _, o := range olds {
		if cj.budget.fits(need) {
			break
		}
		cj.dropFn(o.idx, cj.fns[o.idx])
	}
}

// chainLen is the number of codes of n's chain.
func chainLen(n *nativeFn) int {
	k := 0
	for ; n != nil; n = n.prev {
		k++
	}
	return k
}

// relinked returns n with the codes it replaced kept, in order, those of
// its chain: a copy, n being shared with the readers that loaded it.
func (n *nativeFn) relinked(keep []*nativeFn) *nativeFn {
	var prev *nativeFn
	for i := len(keep) - 1; i >= 0; i-- {
		k := *keep[i]
		k.prev = prev
		prev = &k
	}
	m := *n
	m.prev = prev
	return &m
}

// dropFn drops function idx's code, which no frame is in: its next call
// compiles it again. The run lock is held for writing and cj.mu.
func (cj *codeJIT) dropFn(idx int, f *fnJIT) {
	nat := f.native.Load()
	atomic.StoreUintptr(&cj.links[idx], 0)
	f.native.Store(nil)
	opt := false
	for p := nat; p != nil; p = p.prev {
		opt = opt || p.opt
		cj.drop(p.place)
	}
	if opt {
		// Optimized again once it is hot again.
		f.optDone.Store(false)
	}
	cj.dropped.Add(1)
	jitStats.dropped.Add(1)
}

// jitExecs are the execs made, held weakly: those a pass that drops code
// reads the levels of (pins). An exec is made when none is free for a call,
// so those alive are as many as the calls that ran at once, at most, and
// the pool's; one the collector freed ran nothing. Those freed are swept
// out once the list has doubled since the last sweep (sweepAt), so that
// making execs costs no more than a constant each however many the pool
// lets go of.
var jitExecs struct {
	mu      sync.Mutex
	list    []weak.Pointer[exec]
	sweepAt int
}

// jitTrack adds a new exec to jitExecs.
func jitTrack(x *exec) {
	jitExecs.mu.Lock()
	if len(jitExecs.list) >= jitExecs.sweepAt {
		list := jitExecs.list[:0]
		for _, w := range jitExecs.list {
			if w.Value() != nil {
				list = append(list, w)
			}
		}
		clear(jitExecs.list[len(list):])
		jitExecs.list = list
		jitExecs.sweepAt = max(64, 2*len(list))
	}
	jitExecs.list = append(jitExecs.list, weak.Make(x))
	jitExecs.mu.Unlock()
}
