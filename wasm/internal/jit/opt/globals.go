package opt

// forwardGlobals forwards what a mutable global holds from block to block:
// a read of a global whose value is known where it runs -- the value the
// last write stored, or the last read loaded, on every path to it with
// nothing between that may change the global -- becomes that value. The
// builder knows the globals within a block; this pass carries them across
// the blocks, as the builder carries the locals, by Braun et al.'s
// construction: each global a variable, its value at a block's start
// looked up through the predecessors, a phi made where they bring
// different values.
//
// What may change a global is a call or an instruction Go runs, and,
// for another thread's sake, an atomic access or a fence: after one, the
// value is unknown and the next read loads it. Writes stay where they are,
// so the global always holds what the interpreter's would; only reads go.
//
// A loop that makes no call nor anything else that may change a global
// carries it: at its header the global holds the value its preheader
// brings, and, where the loop writes it, a phi of that and the values its
// back edges bring. Where the loop reads the global and its preheader
// knows no value, a load is put at the preheader's end, run once for the
// loop where the loop's reads ran on each turn. Elsewhere a value one
// predecessor does not know is not known: a load is not put on a path
// that had none.
func forwardGlobals(f *Func) {
	if f.noForward {
		return
	}
	ws := f.ws
	fw := &ws.fw
	fw.reset(f)
	n := int(f.nextBlock)
	fw.blocks = grown(fw.blocks, n)
	fw.loops = grown(fw.loops, n)
	clear(fw.blocks)
	clear(fw.loops)
	for _, b := range f.Blocks {
		l := &fw.blocks[b.ID]
		for _, v := range b.Values {
			switch {
			case v.Op == OpGlobalGet && v.Aux2 != 1:
				if gi := fw.track(v.Aux, v.Args[0], v.Type); gi >= 0 {
					l.reads |= 1 << gi
				}
			case v.Op == OpGlobalSet:
				if gi := fw.track(v.Aux, v.Args[0], v.Args[1].Type); gi >= 0 {
					l.writes |= 1 << gi
				}
			case killsGlobals(v):
				l.kills = true
			}
		}
	}
	// The loops as the builder made them: a loop's blocks are those made
	// from its header to its end, among them those of its natural loop.
	// Without a loop that may carry a global it reads there is nothing to
	// do.
	carries := false
	for _, lr := range ws.loopRanges {
		h := lr.header
		if h.dead {
			continue
		}
		l := fwLoop{header: true}
		for _, bl := range fw.blocks[h.ID:lr.end] {
			l.reads |= bl.reads
			l.writes |= bl.writes
			l.kills = l.kills || bl.kills
		}
		fw.loops[h.ID] = l
		carries = carries || !l.kills && l.reads != 0
	}
	if !carries {
		fw.done()
		return
	}
	fw.rpo = grown(fw.rpo, n)
	fw.state = grown(fw.state, n)
	clear(fw.state)
	for i, b := range f.Blocks {
		fw.rpo[b.ID] = int32(i)
	}
	for _, b := range f.Blocks {
		fw.state[b.ID] |= fwVisited
		if fw.allFilled(b) {
			fw.seal(b)
		}
		for _, v := range b.Values {
			switch {
			case v.Op == OpGlobalGet && v.Aux2 != 1:
				gi := fw.index(v.Aux)
				if gi < 0 {
					continue
				}
				if cur := fw.read(gi, b); cur != &unknownGlobal && cur != v {
					v.setCopy(cur)
					f.forwarded = true
					continue
				}
				fw.defs.set(fwKey(gi, b), v)
			case v.Op == OpGlobalSet:
				if gi := fw.index(v.Aux); gi >= 0 {
					fw.defs.set(fwKey(gi, b), v.Args[1])
				}
			case killsGlobals(v):
				for gi := range fw.gs {
					fw.defs.set(fwKey(gi, b), &unknownGlobal)
				}
			}
		}
		fw.state[b.ID] |= fwFilled
		for _, s := range b.Succs {
			if fw.state[s.ID]&(fwSealed|fwVisited) == fwVisited && fw.allFilled(s) {
				fw.seal(s)
			}
		}
	}
	if len(fw.incomplete) > 0 {
		panic("wasm opt: a loop's phi of a global left incomplete")
	}
	fw.done()
}

// killsGlobals reports whether v may change a global, or make another
// thread's change seen: a call, an instruction Go runs, an atomic access, a
// fence.
func killsGlobals(v *Value) bool {
	return v.IsCall() || isAtomic(v.Op) || v.Op == OpFence
}

// unknownGlobal is the value of a global no read may take.
var unknownGlobal Value

// The blocks' states in forwardGlobals: every predecessor's values known
// (sealed), its own (filled), and its values walked or being walked
// (visited).
const (
	fwSealed = 1 << iota
	fwFilled
	fwVisited
)

func fwKey(gi int, b *Block) uint64 { return uint64(b.ID)<<32 | uint64(gi) }

// fwLoop is what a block, or a loop, does to the globals tracked: those it
// reads and writes, by bit, and whether it may change any; header says a
// loop's.
type fwLoop struct {
	reads, writes uint64
	kills, header bool
}

// loopRange is a loop as the builder made it: its header, and the blocks
// made from it up to end, by ID.
type loopRange struct {
	header *Block
	end    int32
}

// fwState is forwardGlobals' state, kept in the workspace: the globals
// tracked, at most 64 (gs, each one's address and type), each block's
// position in the order, state and doings, each loop header's loop, the
// values at the blocks' ends, and the incomplete phis.
type fwState struct {
	f          *Func
	gs         []uint32
	addrs      []*Value
	types      []Type
	rpo        []int32
	state      []uint8
	blocks     []fwLoop
	loops      []fwLoop
	defs       u64map[*Value]
	incomplete []fwPhi
}

// fwPhi is a loop header's phi of global gi, whose argument from the
// preheader pre is a.
type fwPhi struct {
	gi  int
	phi *Value
	pre *Block
	a   *Value
}

func (fw *fwState) reset(f *Func) {
	*fw = fwState{f: f, gs: fw.gs[:0], addrs: fw.addrs[:0], types: fw.types[:0], rpo: fw.rpo, state: fw.state,
		blocks: fw.blocks, loops: fw.loops, defs: fw.defs, incomplete: fw.incomplete[:0]}
	fw.defs.reset(16)
}

// done drops the values the state holds.
func (fw *fwState) done() {
	clear(fw.addrs[:cap(fw.addrs)])
	clear(fw.incomplete[:cap(fw.incomplete)])
	fw.f = nil
}

// track tracks global g, at addr, of type t, if there is room, and returns
// its index among those tracked, or -1.
func (fw *fwState) track(g uint64, addr *Value, t Type) int {
	if i := fw.index(g); i >= 0 || len(fw.gs) == 64 {
		return i
	}
	fw.gs = append(fw.gs, uint32(g))
	fw.addrs = append(fw.addrs, addr)
	fw.types = append(fw.types, t)
	return len(fw.gs) - 1
}

// index is global g's among those tracked, or -1.
func (fw *fwState) index(g uint64) int {
	for i, x := range fw.gs {
		if uint64(x) == g {
			return i
		}
	}
	return -1
}

func (fw *fwState) allFilled(b *Block) bool {
	for _, p := range b.Preds {
		if fw.state[p.ID]&fwFilled == 0 {
			return false
		}
	}
	return true
}

// preheader reports whether b is a loop's header -- a predecessor of it
// comes at or after it in the order -- and returns its one predecessor
// before it, or nil where it has several.
func (fw *fwState) preheader(b *Block) (pre *Block, header bool) {
	several := false
	for _, p := range b.Preds {
		switch {
		case fw.rpo[p.ID] >= fw.rpo[b.ID]:
			header = true
		case pre == nil:
			pre = p
		default:
			several = true
		}
	}
	if several {
		pre = nil
	}
	return pre, header
}

// read is the value of global gi in block b where the walk is, or at its
// end if b was walked: unknownGlobal where no read may take it.
func (fw *fwState) read(gi int, b *Block) *Value {
	k := fwKey(gi, b)
	if v, ok := fw.defs.get(k); ok {
		return v
	}
	var v *Value
	pre, header := fw.preheader(b)
	switch {
	case len(b.Preds) == 0:
		v = &unknownGlobal
	case header:
		v = fw.atHeader(gi, b, pre)
	case len(b.Preds) == 1:
		v = fw.read(gi, b.Preds[0])
	default:
		phi := fw.f.newValue(b, OpPhi, fw.types[gi])
		fw.defs.set(k, phi)
		for _, p := range b.Preds {
			a := fw.read(gi, p)
			if a == &unknownGlobal {
				// No lookup the phi began came back to it, which only
				// one through a loop's back edge can, into a loop whose
				// values are all known: it is the last phi made.
				if b.Phis[len(b.Phis)-1] != phi {
					panic("wasm opt: a global's phi not the last made")
				}
				phi.Args = nil
				b.Phis = b.Phis[:len(b.Phis)-1]
				v = &unknownGlobal
				break
			}
			fw.f.addArg(phi, a)
		}
		if v == nil {
			v = phi
		}
	}
	fw.defs.set(k, v)
	return v
}

// atHeader is the value of global gi where loop header b, whose preheader
// is pre, begins.
func (fw *fwState) atHeader(gi int, b, pre *Block) *Value {
	l := &fw.loops[b.ID]
	bit := uint64(1) << gi
	if !l.header || pre == nil || l.kills {
		return &unknownGlobal
	}
	a := fw.read(gi, pre)
	if a == &unknownGlobal {
		if l.reads&bit == 0 {
			return a
		}
		if a = fw.loadAtEnd(gi, pre); a == nil {
			return &unknownGlobal
		}
	}
	if l.writes&bit == 0 {
		return a
	}
	phi := fw.f.newValue(b, OpPhi, fw.types[gi])
	fw.defs.set(fwKey(gi, b), phi)
	ip := fwPhi{gi, phi, pre, a}
	if fw.state[b.ID]&fwSealed == 0 {
		fw.incomplete = append(fw.incomplete, ip)
		return phi
	}
	fw.complete(ip)
	return phi
}

// complete gives a loop header's phi its arguments, every predecessor
// walked: the back edges', which the loop, changing no global, knows.
func (fw *fwState) complete(ip fwPhi) {
	for _, p := range ip.phi.Block.Preds {
		a := ip.a
		if p != ip.pre {
			a = fw.read(ip.gi, p)
		}
		if a == &unknownGlobal {
			panic("wasm opt: a global unknown in a loop that changes none")
		}
		fw.f.addArg(ip.phi, a)
	}
}

// seal completes b's incomplete phis, every predecessor walked.
func (fw *fwState) seal(b *Block) {
	fw.state[b.ID] |= fwSealed
	for i := 0; i < len(fw.incomplete); i++ {
		ip := fw.incomplete[i]
		if ip.phi.Block != b {
			continue
		}
		fw.incomplete = append(fw.incomplete[:i], fw.incomplete[i+1:]...)
		i--
		fw.complete(ip)
	}
}

// loadAtEnd puts a read of global gi at the end of block p, before the
// condition its branch takes, and returns it, or nil where p ends with a
// call.
func (fw *fwState) loadAtEnd(gi int, p *Block) *Value {
	if p.Kind != BPlain && p.Kind != BIf && p.Kind != BSwitch {
		return nil
	}
	f := fw.f
	v := f.value(p, OpGlobalGet, fw.types[gi], []*Value{fw.addrs[gi]})
	v.Aux = uint64(fw.gs[gi])
	i := len(p.Values)
	if i > 0 && p.Control == p.Values[i-1] {
		i--
	}
	p.Values = appendA(&f.ws.vals, p.Values, nil)
	copy(p.Values[i+1:], p.Values[i:])
	p.Values[i] = v
	fw.defs.set(fwKey(gi, p), v)
	return v
}
