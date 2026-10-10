package vm

import (
	"github.com/go-quickjs/go-quickjs/internal/arena"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/graph"
	"github.com/go-quickjs/go-quickjs/internal/pool"
)

// Structured control flow for the tree tier.
//
// buildTree makes a function's tree a list of basic blocks, which runTree
// runs one at a time: a block's statements, then its end, which chooses the
// next block. Every jump, the back edge of every loop included, is then a
// call of the end and a trip round runTree's loop. A flow rebuilds the
// loops of the blocks' graph as the structures the source had, as a
// decompiler would: a block that only one other can go on to becomes part of
// it, run in place; a block one end of a test goes to, and nothing else, is
// one arm of an if in the test's block; a block that goes back to itself is
// a Go loop. What it cannot rebuild -- code no structure fits, and anything
// outside a loop, where a structure would save no more than it costs -- is
// left to runTree's dispatch, from block to block, as before.
//
// Nothing is run that was not run before, nor in another order: the
// structures call the same statements and ends, the tests and the returns
// with them, and only an unconditional jump, whose end did nothing but count
// a back edge and name its target, is done in place. A structure's result is
// the block to go on to, as an end's is, or -1 to return, so anything that
// leaves one goes where it went before, and where that is a block no longer
// run by itself, runTree runs it, unchanged, from its own entry.
//
// The structures are built as data first (fseg) and made closures last
// (emitSegs): a structure that can only go on to what follows it -- an if
// whose arms meet again, a loop with one way out -- becomes a statement of
// the code around it, and a loop whose body is then tests and straight code
// (loopW, loopTests) runs as one Go for, the tests the only calls besides
// the statements.
//
// A flow keeps all it works in between builds, in flowPool: the block
// graph's orders, dominators and loops (internal/graph), and arenas
// (internal/arena) its steps, arms and lists of blocks are cut from. Warm,
// it allocates what the structures keep -- their closures and the
// statement lists they run -- and nothing else. Block indices are dense,
// so everything is arrays indexed by them; what a block can go on to is a
// short list, in the order it was found, which is the order the rebuilding
// tries them in.
//
// It is in the package's last file so that its code sits after everything
// else's.

// fstep is straight-line code and the way out of it: the statements, then
// the block's own end, or a loop of steps, run while they go on to head, or
// else a jump to to, which counts a back edge at pc from a stack depth of
// depth where back says it goes back. raw is what the way out can go on to,
// -1 being a return.
type fstep struct {
	body      []tstmt
	end       tnext
	loop      []fseg
	head      int
	to        int
	back      bool
	pc, depth int
	raw       []int32
	// cmp is end's comparison, where a loop can make it in place.
	cmp *tcmp
	// step, where it is not nil, says that body's last statement steps a
	// local by one, which a loop can do in place. A body only ever gains
	// statements before its own, so the statement stays last.
	step *tstep
}

// jump reports whether the step's way out is a jump.
func (s *fstep) jump() bool { return s.end == nil && s.loop == nil }

// fseg is a step of a structure, the arms its result chooses between, and
// the result that goes on to the next step: any other leaves the structure.
// res is what it can go on to, through its arms, once lowerSegs has found
// it.
type fseg struct {
	fstep
	arms []farm
	cont int
	res  []int32
}

// farm is code run where a step's result is at: the arm of an if, whose
// own result then stands for the step's. res is what its steps can go on
// to, once lowerSegs has found it.
type farm struct {
	at   int
	segs []fseg
	res  []int32
}

// tflow is a block of the graph a flow rebuilds: the steps it runs, and
// what it can go on to.
type tflow struct {
	segs []fseg
	// raw is what the last step's way out can choose, which an arm can be
	// put on; res is what the last step can go on to, its arms' results
	// among them; out is what the steps before the last can leave the block
	// for; succ is everything the block can go on to; ret says it can
	// return. raw is only ever replaced, never changed, and may be shared;
	// the others are the block's own, changed in place.
	raw, res, out, succ []int32
	ret                 bool
	// level is the innermost loop the block is in, by its header, or -1.
	level   int
	changed bool
	dead    bool
}

// How a block ends, where a flow needs to know.
const (
	endFalls      = 1 + iota // it runs on into the next block
	endThrows                // it throws
	endCompletion            // a finally's record may return or enter an outer finally
)

// flow is the structuring of one function's blocks, and all it works in,
// which flowPool keeps for the next function.
type flow struct {
	// What buildTree records of each block: its successors,
	// succs[start[i]:start[i]+count[i]], how it ends, and the comparison it
	// ends with, where cmp[i].op is not 0.
	start, count []int32
	succs        []int32
	how          []uint8
	cmp          []tcmp
	steps        []tstep
	// entries are catch and finally blocks entered by the exception runner.
	entries []thandlerEntry
	entry   int

	g      graph.DiGraph
	nodes  []tflow
	preds  []int32
	parent []int32
	header []bool
	// owned counts the blocks left in each loop, by its header.
	owned []int32
	// late allows wrap, once nothing else applies.
	late bool
	// costly is set where a block's structure needs a closure that runs
	// structure rather than statements -- arms chosen by runArms, steps in
	// sequence, a loop of steps -- which costs more than runTree's dispatch
	// it replaces: the block is then left as it was, its blocks run by
	// runTree as before. A loop made a statement of the code around it, and
	// a loop's exits, which run once a loop, do not count.
	costly bool
	// The arenas lists of blocks, steps and arms are cut from. Lists are
	// always filled before they are read, so ints is not cleared between
	// builds.
	ints arena.Arena[int32]
	segA arena.Arena[fseg]
	armA arena.Arena[farm]
	// tests is loopShape's.
	tests []wtest
}

// flowPool keeps flows between builds. A function's tree is built by
// whichever goroutine first calls it, and one compiled program may be run
// by runtimes on several goroutines, so the flows are kept where any
// goroutine may take one: a few in an allocator's list, which collections
// leave alone, so that a build finds a flow grown to its size, and any
// more, from builds at once, in a pool.
var flowPool = pool.NewAllocator(4, func() *flow { return new(flow) }, (*flow).reset)

// takeFlow is a flow for a function of n blocks, for buildTree to record
// them in.
func takeFlow(n int) *flow {
	f := flowPool.Get()
	f.start = grow32(f.start, n)
	f.count = grow32(f.count, n)
	f.succs = f.succs[:0]
	f.entries = f.entries[:0]
	f.entry = 0
	if cap(f.how) < n {
		f.how = make([]uint8, n)
		f.cmp = make([]tcmp, n)
		f.steps = make([]tstep, n)
	}
	f.how, f.cmp, f.steps = f.how[:n], f.cmp[:n], f.steps[:n]
	return f
}

// grow32 is s with length n, its contents unspecified.
func grow32(s []int32, n int) []int32 {
	if cap(s) < n {
		return make([]int32, n, n+n/2)
	}
	return s[:n]
}

// record keeps what buildTree built of block i: what it goes on to, how it
// ends, the comparison it ends with, and the step its last statement is.
func (f *flow) record(i int, succ []int, how uint8, cmp tcmp, step tstep) {
	f.start[i], f.count[i] = int32(len(f.succs)), int32(len(succ))
	for _, s := range succ {
		f.succs = append(f.succs, int32(s))
	}
	f.how[i], f.cmp[i], f.steps[i] = how, cmp, step
}

// succOf is what block i goes on to, as buildTree recorded it.
func (f *flow) succOf(i int) []int32 { return f.succs[f.start[i] : f.start[i]+f.count[i]] }

// reset clears what f holds of the function it structured, so that the
// pool keeps nothing of it alive.
func (f *flow) reset() {
	clear(f.nodes)
	clear(f.cmp)
	clear(f.steps)
	clear(f.tests)
	f.tests = f.tests[:0]
	f.ints.Forget()
	f.segA.Rewind()
	f.armA.Rewind()
}

// has reports whether l holds x.
func has(l []int32, x int) bool {
	for _, y := range l {
		if int(y) == x {
			return true
		}
	}
	return false
}

// add is l with x at its end, if it was not in it.
func (f *flow) add(l []int32, x int) []int32 {
	if has(l, x) {
		return l
	}
	return f.ints.Append(l, int32(x))
}

// del is l without x, the others in their order, in place.
func del(l []int32, x int) []int32 {
	for i, y := range l {
		if int(y) == x {
			copy(l[i:], l[i+1:])
			return l[:len(l)-1]
		}
	}
	return l
}

// copyList is a list of its own with l's blocks.
func (f *flow) copyList(l []int32) []int32 {
	m := f.ints.Make(len(l) + 2)[:len(l)]
	copy(m, l)
	return m
}

// structure rebuilds structured control flow in t's blocks, of which done
// says which are ever run, and gives f back to the pool.
func (f *flow) structure(t *tree, done []bool) {
	defer flowPool.Put(f)
	n := len(t.blocks)
	// Only loops are rebuilt, and a loop goes back to a block no later in
	// the code than the one it goes back from: code with no such jump is
	// left as it is, without looking further.
	loops := false
	for i := range n {
		if done[i] {
			for _, y := range f.succOf(i) {
				loops = loops || int(y) <= i
			}
		}
	}
	if !loops {
		return
	}
	if cap(f.nodes) < n {
		f.nodes = make([]tflow, n)
	}
	f.nodes = f.nodes[:n]
	f.preds = grow32(f.preds, n)
	clear(f.preds)
	f.g.Reset(n)
	nodes := f.nodes
	for i := range nodes {
		nd := &nodes[i]
		if !done[i] {
			nd.dead = true
			continue
		}
		blk := &t.blocks[i]
		// The successors, each once, in the order the block found them: the
		// graph's, which nothing changes.
		raw := f.succOf(i)
		if f.how[i] == endCompletion {
			raw = f.completionSuccessors(i, raw)
		}
		if len(raw) > 1 {
			var u []int32
			for _, y := range raw {
				u = f.add(u, int(y))
			}
			raw = u
		}
		f.g.SetSuccessors(int32(i), raw)
		raw = f.g.Successors(int32(i))
		// The block's one step, filled in where the arena made it, which is
		// zeroed: no step is built to be copied there.
		nd.segs = f.segA.Make(1)
		st := &nd.segs[0]
		st.body, st.end, st.raw, st.cont = blk.body, blk.next, raw, -2
		if f.cmp[i].op != 0 {
			st.cmp = &f.cmp[i]
		}
		if f.steps[i].ok && len(st.body) > 0 {
			st.step = &f.steps[i]
		}
		if len(raw) == 0 && f.how[i] != endThrows {
			st.raw, nd.ret = f.add(nil, -1), true
		}
		if f.how[i] == endCompletion {
			st.raw, nd.ret = f.add(f.copyList(raw), -1), true
		}
		if blk.jump != 0 || f.how[i] == endFalls {
			st.end, st.to, st.back, st.pc, st.depth = nil, int(raw[0]), blk.back, blk.pc, blk.depth
		}
		// res and succ are copies the block changes, each with room to grow.
		two := f.ints.Make(2 * (len(raw) + 2))
		nd.res, nd.succ = two[:len(raw):len(raw)+2], two[len(raw)+2:2*len(raw)+2]
		copy(nd.res, raw)
		copy(nd.succ, raw)
		nd.raw = raw
		for _, y := range raw {
			f.preds[y]++
		}
	}
	// The runner enters this component at its first block.
	f.preds[f.entry]++
	level, parent, header, ok := f.g.LoopNest(int32(f.entry))
	if !ok {
		// Not reducible -- which compiled JavaScript never is: the blocks
		// are left to runTree, as they were.
		return
	}
	order := f.g.Order()
	f.parent, f.header = parent, header
	f.owned = grow32(f.owned, n)
	clear(f.owned)
	for i := range nodes {
		nodes[i].level = int(level[i])
		if l := level[i]; l >= 0 && !nodes[i].dead {
			f.owned[l]++
		}
	}
	// Blocks are taken last first, so that what a block goes on to has been
	// rebuilt when it is.
	f.late = false
	for changed := true; changed; {
		changed = false
		for i := len(order) - 1; i >= 0; i-- {
			x := int(order[i])
			for !nodes[x].dead && f.step(x) {
				changed = true
			}
		}
		if !changed && !f.late {
			// A pass that may wrap, if any block could.
			f.late, changed = true, f.wrappable()
		} else if changed {
			f.late = false
		}
	}
	if flowSeen != nil {
		flowSeen(f, t)
	}
	live, kept := 0, 0
	for i := range nodes {
		nd := &nodes[i]
		if nd.dead {
			continue
		}
		live++
		if !nd.changed {
			continue
		}
		f.costly = false
		segs, _ := f.lowerSegs(nd.segs)
		nd.segs = segs
		// The first step's statements are the block's, which runTree runs.
		body := segs[0].body
		segs[0].body, segs[0].step = nil, nil
		next := f.emitSegs(segs)
		if f.costly {
			kept++
			continue
		}
		t.blocks[i] = tblock{body: body, next: next}
	}
	if flowKept != nil {
		flowKept(kept)
	}
	if live == 1 && kept == 0 && len(nodes[0].succ) == 0 && len(f.entries) == 0 {
		// The whole function is one structure, which runTree runs as
		// straight-line code.
		t.blocks = t.blocks[:1:1]
	}
}

// flowSeen, where a test sets it, is shown each flow once its blocks are
// rebuilt, before they are made closures.
var flowSeen func(f *flow, t *tree)

// flowKept, where a test sets it, is told how many rebuilt blocks a
// structuring left as they were, their structures too costly.
var flowKept func(kept int)

// step makes one change to x where one applies, and reports whether it did.
func (f *flow) step(x int) bool {
	nd := &f.nodes[x]
	// A block that can go on to itself is a loop.
	if has(nd.succ, x) {
		rest := del(nd.succ, x)
		raw := rest
		if nd.ret {
			raw = f.add(f.copyList(rest), -1)
		}
		loop := f.segA.Make(1)
		loop[0] = fseg{fstep: fstep{loop: nd.segs, head: x, raw: raw}, cont: -2}
		nd.segs = loop
		nd.raw, nd.res, nd.out, nd.succ = rest, f.copyList(rest), nil, f.copyList(rest)
		f.preds[x]--
		if nd.level == x {
			nd.level = int(f.parent[x])
			f.owned[x]--
			if nd.level >= 0 {
				f.owned[nd.level]++
			}
		}
		nd.changed = true
		return true
	}
	last := &nd.segs[len(nd.segs)-1]
	// A block an unconditional jump goes to, and nothing else, runs on from
	// the jump.
	if last.jump() && last.arms == nil {
		if y := last.to; f.absorbable(x, y) && !has(nd.out, y) {
			f.absorb(x, y)
			return true
		}
	}
	if nd.level < 0 && f.owned[x] == 0 {
		// Outside loops -- and outside what is left of a loop whose header,
		// shared with a loop inside it, x is -- a structure saves no more
		// than runTree's trip from block to block, and its closure and arms
		// cost as much, once a call: tests and their arms there are left to
		// runTree.
		return false
	}
	// A block one way out of a test goes to, and nothing else, is an arm:
	// one that goes on to one place, or nowhere; anything bigger where it
	// goes on only to where the test's other way goes, as an if's arm does.
	// A block of an outer loop that goes back to that loop's header is
	// left to it, so that the inner loop has one way out.
	if !last.jump() && len(nd.raw) == 2 {
		for i := range 2 {
			a, other := int(nd.raw[i]), int(nd.raw[1-i])
			if !f.armable(x, a) || hasArm(last.arms, a) || has(nd.out, a) || f.armsReach(last, a) {
				continue
			}
			an := &f.nodes[a]
			if f.reachesArm(an, last) {
				continue
			}
			if an.level != nd.level {
				outer := false
				for _, z := range an.succ {
					outer = outer || int(z) != x && f.header[z]
				}
				if outer {
					continue
				}
			}
			ok := len(an.succ) <= 1
			if !ok {
				// Where the other way is an arm already, where it goes is
				// the step's result.
				joins := nd.res
				if !hasArm(last.arms, other) {
					joins = f.nodes[other].succ
				}
				ok = true
				for _, z := range an.succ {
					if int(z) != other && !has(joins, int(z)) {
						ok = false
					}
				}
			}
			if ok {
				f.arm(x, a)
				return true
			}
		}
	}
	for _, y := range nd.res {
		if y := int(y); f.absorbable(x, y) && !has(nd.out, y) {
			f.absorb(x, y)
			return true
		}
	}
	if f.late {
		// A block that only x goes on to, but from more than one place in
		// it: x is run as a step, whose result goes on to the block.
		for _, y := range nd.out {
			if y := int(y); f.absorbable(x, y) {
				f.wrap(x, y)
				return true
			}
		}
	}
	return false
}

// wrappable reports whether a pass with wrap allowed might do anything:
// whether a block step goes on to wrap in has steps that leave it.
func (f *flow) wrappable() bool {
	for x := range f.nodes {
		if nd := &f.nodes[x]; !nd.dead && len(nd.out) > 0 && (nd.level >= 0 || f.owned[x] > 0) {
			return true
		}
	}
	return false
}

// wrap makes x's steps one step, which goes on to y, and y part of x.
func (f *flow) wrap(x, y int) {
	nd, yn := &f.nodes[x], &f.nodes[y]
	// The step is an arm of a jump to nowhere.
	arms := f.armA.Make(1)
	arms[0] = farm{at: wrapAt, segs: nd.segs}
	segs := f.segA.Make(1 + len(yn.segs))
	segs[0] = fseg{fstep: fstep{to: wrapAt, raw: f.add(nil, wrapAt)}, arms: arms, cont: y}
	copy(segs[1:], yn.segs)
	nd.segs = segs
	out := del(f.copyList(nd.succ), y)
	for _, z := range yn.out {
		out = f.add(out, int(z))
	}
	nd.out, nd.raw, nd.res = out, yn.raw, yn.res
	f.join(x, y)
}

// wrapAt is the result of wrap's jump to nowhere.
const wrapAt = -3

// armsReach reports whether an arm of seg can go on to a.
func (f *flow) armsReach(seg *fseg, a int) bool {
	for _, arm := range seg.arms {
		if has(f.nodes[arm.at].succ, a) {
			return true
		}
	}
	return false
}

// reachesArm reports whether a block can go on to one of seg's arms, which
// would then be left rather than run.
func (f *flow) reachesArm(an *tflow, seg *fseg) bool {
	for _, z := range an.succ {
		if hasArm(seg.arms, int(z)) {
			return true
		}
	}
	return false
}

func (f *flow) armable(x, y int) bool {
	return y != x && y != 0 && !f.nodes[y].dead && f.preds[y] == 1
}

// absorbable reports whether y may be made part of x: a block in the same
// loop, or in the loop x heads, which a loop with the same header as one
// inside it still has left once the inner one is built.
func (f *flow) absorbable(x, y int) bool {
	l := f.nodes[y].level
	return f.armable(x, y) && (l == f.nodes[x].level || l == x)
}

// absorb makes y, which only x goes on to, part of x, run where x's last
// step goes on to it.
func (f *flow) absorb(x, y int) {
	nd, yn := &f.nodes[x], &f.nodes[y]
	last := &nd.segs[len(nd.segs)-1]
	first := &yn.segs[0]
	if last.jump() && !last.back && last.arms == nil && last.to == y {
		// Straight on: one step.
		body := append(last.body[:len(last.body):len(last.body)], first.body...)
		*last = *first
		last.body = body
		if len(yn.segs) > 1 {
			segs := f.segA.Make(len(nd.segs) + len(yn.segs) - 1)
			copy(segs[copy(segs, nd.segs):], yn.segs[1:])
			nd.segs = segs
		}
	} else {
		last.cont = y
		segs := f.segA.Make(len(nd.segs) + len(yn.segs))
		copy(segs[copy(segs, nd.segs):], yn.segs)
		nd.segs = segs
		for _, z := range nd.res {
			if int(z) != y {
				nd.out = f.add(nd.out, int(z))
			}
		}
	}
	for _, z := range yn.out {
		nd.out = f.add(nd.out, int(z))
	}
	nd.raw, nd.res = yn.raw, yn.res
	f.join(x, y)
}

// arm makes a, which only x goes on to, an arm of x's last step.
func (f *flow) arm(x, a int) {
	nd, an := &f.nodes[x], &f.nodes[a]
	last := &nd.segs[len(nd.segs)-1]
	arms := f.armA.Make(len(last.arms) + 1)
	copy(arms, last.arms)
	arms[len(last.arms)] = farm{at: a, segs: an.segs}
	last.arms = arms
	nd.res = del(nd.res, a)
	for _, z := range an.succ {
		nd.res = f.add(nd.res, int(z))
	}
	f.join(x, a)
}

// join gives x what y went on to, y being part of it now.
func (f *flow) join(x, y int) {
	nd, yn := &f.nodes[x], &f.nodes[y]
	nd.succ = del(nd.succ, y)
	for _, z := range yn.succ {
		if has(nd.succ, int(z)) {
			f.preds[z]--
		} else {
			nd.succ = f.add(nd.succ, int(z))
		}
	}
	nd.ret = nd.ret || yn.ret
	nd.changed = true
	yn.dead = true
	if yn.level >= 0 {
		f.owned[yn.level]--
	}
}

// segResults is what a step can go on to, through its arms, whose results
// lowerSegs has found.
func (f *flow) segResults(s *fseg) []int32 {
	r := f.ints.Make(len(s.raw) + 2)[:0]
	for _, z := range s.raw {
		if !hasArm(s.arms, int(z)) {
			r = f.add(r, int(z))
		}
	}
	for _, a := range s.arms {
		for _, z := range a.res {
			r = f.add(r, int(z))
		}
	}
	return r
}

// lowerSegs makes each step that can only go on to the next a statement of
// it, in its arms and loops too: an if whose arms meet again, a loop with
// one way out, a jump that only counts a back edge. It works in place, the
// steps left a prefix of segs, which it reports with what they can go on
// to, which making steps statements does not change; it keeps what each
// step and arm can go on to in its res. Steps already lowered are lowered
// again to themselves.
func (f *flow) lowerSegs(segs []fseg) ([]fseg, []int32) {
	out := segs[:0]
	all := f.ints.Make(4)[:0]
	var pending []tstmt
	for i := range segs {
		s := &segs[i]
		for j := range s.arms {
			a := &s.arms[j]
			a.segs, a.res = f.lowerSegs(a.segs)
		}
		if s.loop != nil {
			s.loop, _ = f.lowerSegs(s.loop)
		}
		if pending != nil {
			s.body = append(pending, s.body...)
			pending = nil
		}
		s.res = f.segResults(s)
		last := i == len(segs)-1
		for _, z := range s.res {
			if last || int(z) != s.cont {
				all = f.add(all, int(z))
			}
		}
		if !last && len(s.res) == 1 && int(s.res[0]) == s.cont {
			// The statements, the step's way out as one, and room for the
			// next step's statements, which follow them.
			pending = make([]tstmt, len(s.body), len(s.body)+1+len(segs[i+1].body))
			copy(pending, s.body)
			if st := f.stmtOf(s); st != nil {
				pending = append(pending, st)
			}
			continue
		}
		out = append(out, *s)
	}
	return out, all
}

// stmtOf is the way out of s and its arms as a statement, or nil for none,
// s going on only to what follows it.
func (f *flow) stmtOf(s *fseg) tstmt {
	if s.jump() && s.arms == nil {
		if !s.back {
			return nil
		}
		pc, d := s.pc, s.depth
		return func(c *tctx) { c.backEdge(pc, d) }
	}
	if s.end != nil && len(s.arms) <= 2 {
		end := s.end
		var a [2]tarm
		simple := true
		for i, arm := range s.arms {
			a[i] = f.armOf(arm)
			simple = simple && a[i].f == nil && a[i].end == nil
		}
		switch {
		case len(s.arms) == 0:
			return func(c *tctx) { end(c) }
		case simple && len(s.arms) == 1:
			a := a[0]
			return func(c *tctx) {
				if end(c) == a.at {
					for _, s := range a.body {
						s(c)
					}
					if a.back {
						c.backEdge(a.pc, a.depth)
					}
				}
			}
		case simple:
			a, b := a[0], a[1]
			return func(c *tctx) {
				switch end(c) {
				case a.at:
					for _, s := range a.body {
						s(c)
					}
					if a.back {
						c.backEdge(a.pc, a.depth)
					}
				case b.at:
					for _, s := range b.body {
						s(c)
					}
					if b.back {
						c.backEdge(b.pc, b.depth)
					}
				}
			}
		}
	}
	// A loop as a statement costs a call each time the loop is entered,
	// not each time round it.
	if s.loop == nil || s.arms != nil {
		f.costly = true
	}
	way := *s
	way.body, way.step = nil, nil
	g := f.emitSeg(&way)
	return func(c *tctx) { g(c) }
}

// tarm is an arm as it is run: the statements and a jump, or f, a structure
// to run in their place.
type tarm struct {
	at        int
	body      []tstmt
	to        int
	back      bool
	pc, depth int
	f         tnext
	// end, where it is not nil, is the arm's block's own end, which ends
	// it in place of the jump.
	end tnext
}

// armOf makes a's code an arm to run.
func (f *flow) armOf(a farm) tarm {
	if len(a.segs) == 1 {
		if s := &a.segs[0]; s.jump() && s.arms == nil {
			return tarm{at: a.at, body: s.body, to: s.to, back: s.back, pc: s.pc, depth: s.depth}
		} else if s.end != nil && s.arms == nil {
			return tarm{at: a.at, body: s.body, end: s.end}
		}
	}
	return tarm{at: a.at, f: f.emitSegs(a.segs)}
}

// armsOf makes arms to run of arms.
func (f *flow) armsOf(arms []farm) []tarm {
	if len(arms) == 0 {
		return nil
	}
	out := make([]tarm, len(arms))
	for i, a := range arms {
		out[i] = f.armOf(a)
	}
	return out
}

// runArms runs the arm n chooses, if any, and reports where it goes on to.
func (c *tctx) runArms(arms []tarm, n int) int {
	for i := range arms {
		a := &arms[i]
		if n != a.at {
			continue
		}
		if a.f != nil {
			return a.f(c)
		}
		for _, s := range a.body {
			s(c)
		}
		if a.end != nil {
			return a.end(c)
		}
		if a.back {
			c.backEdge(a.pc, a.depth)
		}
		return a.to
	}
	return n
}

// emitSegs makes segs a closure.
func (f *flow) emitSegs(segs []fseg) tnext {
	if len(segs) == 1 {
		return f.emitSeg(&segs[0])
	}
	f.costly = true
	fs := make([]tnext, len(segs))
	conts := make([]int, len(segs))
	for i := range segs {
		fs[i], conts[i] = f.emitSeg(&segs[i]), segs[i].cont
	}
	if len(fs) == 2 {
		g, h, k := fs[0], fs[1], conts[0]
		return func(c *tctx) int {
			if n := g(c); n != k {
				return n
			}
			return h(c)
		}
	}
	return func(c *tctx) int {
		for i, g := range fs {
			if n := g(c); n != conts[i] {
				return n
			}
		}
		return -1
	}
}

// emitSeg makes a step and its arms a closure.
func (f *flow) emitSeg(s *fseg) tnext {
	way := s.end
	if s.loop != nil {
		way = f.emitLoop(s.loop, s.head)
	}
	body := s.body
	arms := f.armsOf(s.arms)
	switch {
	case way == nil && arms == nil:
		to, back, pc, d := s.to, s.back, s.pc, s.depth
		return func(c *tctx) int {
			for _, s := range body {
				s(c)
			}
			if back {
				c.backEdge(pc, d)
			}
			return to
		}
	case way == nil:
		f.costly = true
		to, back, pc, d := s.to, s.back, s.pc, s.depth
		return func(c *tctx) int {
			for _, s := range body {
				s(c)
			}
			if back {
				c.backEdge(pc, d)
			}
			return c.runArms(arms, to)
		}
	case arms == nil && body == nil:
		return way
	case arms == nil:
		return func(c *tctx) int {
			for _, s := range body {
				s(c)
			}
			return way(c)
		}
	}
	f.costly = true
	return func(c *tctx) int {
		for _, s := range body {
			s(c)
		}
		return c.runArms(arms, way(c))
	}
}

// emitLoop makes a closure of the loop of body, run while it goes on to
// head. A body that is tests, each going on one way and leaving the loop
// the others, then straight code going back to head, is loopW, loopCmp or
// loopTests; anything else runs body's closure until it goes on to
// anything but head.
func (f *flow) emitLoop(body []fseg, head int) tnext {
	// A loop's exits run once a loop: what they cost does not count.
	exits := func(arms []farm) []tarm {
		costly := f.costly
		t := f.armsOf(arms)
		f.costly = costly
		return t
	}
	base := len(f.tests)
	defer func() { f.tests = f.tests[:base] }()
	if tests, k, ok := f.loopShape(body, head); ok {
		var bb []tstmt
		back, pc, d := false, 0, 0
		if k != nil {
			bb, back, pc, d = k.body, k.back, k.pc, k.depth
		}
		if len(tests) == 1 {
			w := &tests[0]
			// A step at the end of the body is made in place.
			var st tstep
			if k != nil && k.step != nil {
				st = *k.step
				st.s, bb = bb[len(bb)-1], bb[:len(bb)-1:len(bb)-1]
			}
			if w.test.cmp != nil {
				return loopCmp(w.test.body, w.test.end, *w.test.cmp, w.on, bb, st, back, pc, d, exits(w.exits))
			}
			return loopW(w.test.body, w.test.end, w.on, bb, st, back, pc, d, exits(w.exits))
		}
		items := make([]ttest, len(tests))
		for i := range tests {
			w := &tests[i]
			items[i] = ttest{body: w.test.body, test: w.test.end, on: w.on, exits: exits(w.exits)}
		}
		return loopTests(items, bb, back, pc, d)
	}
	f.costly = true
	g := f.emitSegs(body)
	return func(c *tctx) int {
		for {
			if n := g(c); n != head {
				return n
			}
		}
	}
}

// wtest is a test in a loop's body: the step whose end it is, the result
// on which the loop goes on, and the arms its other results leave by.
type wtest struct {
	test  *fseg
	on    int
	exits []farm
}

// loopShape finds, in a loop's body, the tests and the straight code after
// them that loopW and loopTests run: k is the code, or nil where the last
// test itself goes back to head. The tests are on the end of the flow's,
// which emitLoop cuts back once it is done with them, so that a loop it
// emits meanwhile, in an exit, finds its own past them.
func (f *flow) loopShape(body []fseg, head int) (tests []wtest, k *fseg, ok bool) {
	straight := func(k *fseg) bool { return k.jump() && k.arms == nil && k.to == head }
	base := len(f.tests)
	tests = f.tests[base:]
	defer func() { f.tests = append(f.tests[:base], tests...) }()
	for {
		if len(body) == 1 && straight(&body[0]) {
			return tests, &body[0], len(tests) > 0
		}
		s := &body[0]
		if s.end == nil {
			return nil, nil, false
		}
		if len(body) == 1 && len(s.res) == 1 && int(s.res[0]) == head && len(tests) > 0 {
			// Every way out of the last test goes back, as an if's arms
			// that both continue the loop do: it is a statement, and its
			// arms count their own back edges.
			b := make([]tstmt, len(s.body), len(s.body)+1)
			copy(b, s.body)
			k := &fseg{fstep: fstep{body: append(b, f.stmtOf(s)), to: head}}
			return tests, k, true
		}
		w := wtest{test: s, on: -2}
		var rest []fseg
		if len(body) > 1 {
			w.on, w.exits, rest = s.cont, s.arms, body[1:]
		} else {
			for i, a := range s.arms {
				if has(a.res, head) {
					if w.on != -2 {
						return nil, nil, false
					}
					w.on, rest = a.at, a.segs
					w.exits = f.armA.Make(len(s.arms) - 1)
					copy(w.exits[copy(w.exits, s.arms[:i]):], s.arms[i+1:])
				}
			}
			if w.on == -2 {
				if !has(s.raw, head) || hasArm(s.arms, head) {
					return nil, nil, false
				}
				// The test itself goes back.
				w.on, w.exits = head, s.arms
			}
		}
		// What leaves through an arm must leave the loop.
		for _, e := range w.exits {
			if has(e.res, w.on) || has(e.res, head) {
				return nil, nil, false
			}
		}
		tests = append(tests, w)
		if rest == nil {
			return tests, nil, true
		}
		body = rest
	}
}

func hasArm(arms []farm, a int) bool {
	for _, x := range arms {
		if x.at == a {
			return true
		}
	}
	return false
}

// ttest is a test of loopTests: statements, the test, the result on which
// the loop goes on, and the arms that leave it.
type ttest struct {
	body  []tstmt
	test  tnext
	on    int
	exits []tarm
	// next is the test after this one, for loopTests's walk.
	next *ttest
}

// loopTests is loopW with more than one test: each in turn goes on or
// leaves the loop, and the statements bb and the jump back end it.
//
//go:noinline
func loopTests(tests []ttest, bb []tstmt, back bool, pc, d int) tnext {
	if len(tests) == 2 {
		// A loop's test and one more, the commonest, without the loop over
		// the tests, whose state Go keeps on the stack across the calls.
		w := &wloop2{t0: tests[0], t1: tests[1], bb: bb, back: back, pc: pc, d: d}
		return func(c *tctx) int {
			for {
				for _, s := range w.t0.body {
					s(c)
				}
				if n := w.t0.test(c); n != w.t0.on {
					return c.runArms(w.t0.exits, n)
				}
				for _, s := range w.t1.body {
					s(c)
				}
				if n := w.t1.test(c); n != w.t1.on {
					return c.runArms(w.t1.exits, n)
				}
				for _, s := range w.bb {
					s(c)
				}
				if w.back {
					c.backEdge(w.pc, w.d)
				}
			}
		}
	}
	// The tests are walked as a list: the walk's state across each call is
	// then one pointer, where an index into a slice kept the slice too.
	for i := range tests[:len(tests)-1] {
		tests[i].next = &tests[i+1]
	}
	w := &wloop{bb: bb, back: back, pc: pc, d: d, first: &tests[0]}
	return func(c *tctx) int {
		for {
			for t := w.first; t != nil; t = t.next {
				for _, s := range t.body {
					s(c)
				}
				if n := t.test(c); n != t.on {
					return c.runArms(t.exits, n)
				}
			}
			for _, s := range w.bb {
				s(c)
			}
			if w.back {
				c.backEdge(w.pc, w.d)
			}
		}
	}
}

// loopW is a loop of the statements hb, the test test, and, where the test
// gives at, the statements bb and a jump back, which counts a back edge
// where back says so. Any other result of the test leaves the loop, through
// exits.
//
// Its state is one pointer, so that after each call the closure reloads only
// what it uses next.
//
//go:noinline
func loopW(hb []tstmt, test tnext, at int, bb []tstmt, st tstep, back bool, pc, d int, exits []tarm) tnext {
	w := &wloop{hb: hb, bb: bb, step: st, test: test, at: at, back: back, pc: pc, d: d, exits: exits}
	return func(c *tctx) int {
		for {
			for _, s := range w.hb {
				s(c)
			}
			if n := w.test(c); n != w.at {
				return c.runArms(w.exits, n)
			}
			for _, s := range w.bb {
				s(c)
			}
			if st := &w.step; st.ok {
				// tstep's statement's fast path, in place.
				if v := c.locals[st.k]; v.IsNumber() {
					c.locals[st.k] = Float(v.num + st.delta)
				} else {
					st.s(c)
				}
			}
			if w.back {
				c.backEdge(w.pc, w.d)
			}
		}
	}
}

// tcmp is a comparison of a local with a local or a number that ends a
// block, as cmpJump makes it: op, the locals x and y, or the number n where
// num says so, and the blocks it goes on to where it is false and true.
type tcmp struct {
	op          bytecode.Op
	x, y        uint32
	n           Value
	num         bool
	taken, fall int
}

// cmpOf is the comparison cmpJump ends a block with, for a loop to make in
// place, or one whose op is 0. It is kept out of buildBlock, whose size
// moves the code after it.
//
//go:noinline
func cmpOf(op bytecode.Op, x, y tentry, taken, fall int) tcmp {
	switch op {
	case bytecode.OpLt, bytecode.OpLe, bytecode.OpGt, bytecode.OpGe:
	default:
		return tcmp{}
	}
	switch {
	case x.local && y.number:
		return tcmp{op: op, x: x.k, n: y.n, num: true, taken: taken, fall: fall}
	case x.local && y.local:
		return tcmp{op: op, x: x.k, y: y.k, taken: taken, fall: fall}
	}
	return tcmp{}
}

// loopCmp is loopW whose test is k, a comparison of numbers it makes in
// place, as cmpJump's fast path does; where either is not a number, test,
// cmpJump's node, makes it.
//
//go:noinline
func loopCmp(hb []tstmt, test tnext, k tcmp, at int, bb []tstmt, st tstep, back bool, pc, d int, exits []tarm) tnext {
	w := &wloop{hb: hb, bb: bb, step: st, test: test, cmp: k, at: at, back: back, pc: pc, d: d, exits: exits}
	return func(c *tctx) int {
		for {
			for _, s := range w.hb {
				s(c)
			}
			k := &w.cmp
			a, b := c.locals[k.x], k.n
			if !k.num {
				b = c.locals[k.y]
			}
			n := 0
			if a.IsNumber() && b.IsNumber() {
				var t bool
				switch k.op {
				case bytecode.OpLt:
					t = a.num < b.num
				case bytecode.OpLe:
					t = a.num <= b.num
				case bytecode.OpGt:
					t = a.num > b.num
				default:
					t = a.num >= b.num
				}
				n = k.taken
				if t {
					n = k.fall
				}
			} else {
				n = w.test(c)
			}
			if n != w.at {
				return c.runArms(w.exits, n)
			}
			for _, s := range w.bb {
				s(c)
			}
			if st := &w.step; st.ok {
				// tstep's statement's fast path, in place.
				if v := c.locals[st.k]; v.IsNumber() {
					c.locals[st.k] = Float(v.num + st.delta)
				} else {
					st.s(c)
				}
			}
			if w.back {
				c.backEdge(w.pc, w.d)
			}
		}
	}
}

// wloop2 is the state of loopTests's loop of two tests.
type wloop2 struct {
	t0, t1 ttest
	bb     []tstmt
	back   bool
	pc, d  int
}

// wloop is loopW's state.
type wloop struct {
	hb, bb []tstmt
	step   tstep
	test   tnext
	cmp    tcmp
	at     int
	back   bool
	pc, d  int
	exits  []tarm
	first  *ttest
}

// tstep is a statement that steps the local k by delta, ++ or -- whose
// value nothing reads, where ok says there is one: s, which a loop makes
// in place where the local is a number, as s's own fast path does, calling
// s for anything else.
type tstep struct {
	k     uint32
	delta float64
	ok    bool
	s     tstmt
}
