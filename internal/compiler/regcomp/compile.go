package regcomp

import (
	"fmt"
	"math"
	"os"
	"slices"
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/ast"
	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// The register form's frame: r0 the function, r1 its this, the parameters
// from r2, then the function's bindings, then the temporaries
// (docs/register-interpreter.md, "The frame").
const (
	regCallee = 0
	regThis   = 1
	regParams = 2
)

// limit16 is where a 16-bit operand ends.
const limit16 = 1 << 16

// farReg is where slotReg's far bindings start: a slotReg of farReg+k is
// the k-th far binding, whose register is past every near one, reached by
// mov_far and mov_to_far alone (farOf). It is far past any temporary a
// compile that runs out of registers numbers (errRegs), which r16 lets
// through: that compile is made again.
const farReg = 1 << 30

// errRegs is the failure of a function that needs more than 16 bits of
// near registers: compiled again with fewer of its bindings near
// (nearCap).
const errRegs = "more than 65,536 registers"

// Compile compiles lit, whose stack form is fn and whose identifiers' resolution
// is rec, into register code. It returns nil where the function has anything
// the backend does not compile yet; the function then runs as before.
func Compile(lit *ast.FuncLit, fn *bytecode.Function, rec *Record) *bytecode.RegCode {
	if w := why(fn); w != "" && !(w == "arguments" && simpleArguments(fn)) {
		Refuse(fn, w)
		return nil
	}
	return compile(lit, fn, rec, fn)
}

// Later is lit's register code as Compile compiles it, compiled when it is
// first asked for (bytecode.Function.RegCode), or nil where fn's kind
// refuses it at once. The record and the AST are kept until then. The
// code compiled so adds nothing to fn, whose tables are shared by the
// runtimes running it by then: its names, constants and sites are the
// RegCode's (RegCode.Names), the sites numbered past the private members'
// so that the stack form's numbering stays as it is.
//
// A function whose stack form alone has lazySites sites or more is
// compiled at once, while fn is still the compiler's own: its register
// form may need the stack form's sites moved past its own (generate),
// which code running fn's stack form could not see happen.
func Later(lit *ast.FuncLit, fn *bytecode.Function, rec *Record) *bytecode.RegLater {
	if w := why(fn); w != "" && !(w == "arguments" && simpleArguments(fn)) {
		Refuse(fn, w)
		return nil
	}
	if fn.PropSites+fn.PrivateSites >= lazySites {
		rc := compile(lit, fn, rec, fn)
		return bytecode.NewRegLater(func() *bytecode.RegCode { return rc })
	}
	return bytecode.NewRegLater(func() *bytecode.RegCode {
		view := *fn
		view.Names = fn.Names[:len(fn.Names):len(fn.Names)]
		view.Constants = fn.Constants[:len(fn.Constants):len(fn.Constants)]
		view.PropSites = fn.PropSites + fn.PrivateSites
		rc := compile(lit, &view, rec, fn)
		if rc != nil {
			// Never nil, which says the code was compiled with its
			// function (RegCode.Names).
			rc.Names = append(make([]string, 0, len(view.Names)), view.Names...)
			rc.Constants = append(make([]bytecode.Constant, 0, len(view.Constants)), view.Constants...)
			rc.PropSites = view.PropSites
		}
		return rc
	})
}

// compile compiles lit into register code with the tables of fn, which
// are those of orig, the function refusals are recorded for, or a view of
// them that the code's additions go to.
func compile(lit *ast.FuncLit, fn *bytecode.Function, rec *Record, orig *bytecode.Function) *bytecode.RegCode {
	g, ok := generate(fn, fn == orig, func() *gen {
		g := &gen{lit: lit, fn: fn, rec: rec, orig: orig}
		// A derived constructor's this, and an arrow's written inside
		// one, is the binding super() binds, read through the frame
		// (this_bound).
		g.thisRef = fn.Kind == bytecode.KindDerivedConstructor || fn.Kind == bytecode.KindArrow && !fn.ArrowThisValue
		return g
	})
	if !ok {
		Refuse(orig, "its bindings' layout")
		return nil
	}
	if g.err != "" {
		Refuse(orig, g.err)
		return nil
	}
	return g.finish()
}

// generate lays out and compiles the gen made by mk, and reports whether
// its bindings had a layout. A function with more cache sites than 16 bits
// number (errSites) is compiled a second time with the sites of one key
// shared, what the first made of the function's names, constants and
// sites dropped; and where the stack form alone has nearly as many, a
// third time with the register form's sites numbered first, from 1, and
// the stack form's moved past them. That third time is only for own, a
// function the compiler still holds; compiled at its first call
// (Later), a function has fewer than lazySites sites in its stack form.
func generate(fn *bytecode.Function, own bool, mk func() *gen) (*gen, bool) {
	sites, names, consts := fn.PropSites, len(fn.Names), len(fn.Constants)
	last := 1
	if own {
		last = 2
	}
	far, nearCap := false, 0
	for try := 0; ; try++ {
		g := mk()
		g.shareSites = try > 0
		g.ownSites = try > 1
		g.farJumps = far
		g.nearCap = nearCap
		if !g.layout() {
			return nil, false
		}
		g.body()
		if g.err == errJumps && !far {
			// Compiled again, as it was but with its jumps far.
			far = true
			try--
			fn.PropSites, fn.Names, fn.Constants = sites, fn.Names[:names], fn.Constants[:consts]
			continue
		}
		if g.err == errRegs && nearCap == 0 {
			// Compiled again with a quarter of the registers for the
			// bindings used most, and the rest of them far, so that the
			// temporaries have the other three.
			nearCap = limit16 / 4
			try--
			fn.PropSites, fn.Names, fn.Constants = sites, fn.Names[:names], fn.Constants[:consts]
			continue
		}
		if g.err != errSites || try == last {
			if g.ownSites && g.err == "" {
				g.moveStackSites()
			}
			return g, true
		}
		fn.PropSites, fn.Names, fn.Constants = sites, fn.Names[:names], fn.Constants[:consts]
	}
}

// moveStackSites renumbers the stack form's cache sites past the nsites
// the register form numbered from 1: every instruction that has one in its
// B (the compiler's emit and its array literal of constants) and the
// object literals' keys (LiteralKeys). The two forms index one closure's
// caches, and the register form's 16-bit operands reach only the first.
func (g *gen) moveStackSites() {
	n := g.nsites
	fn := g.fn
	for i := range fn.Code {
		in := &fn.Code[i]
		switch in.Op {
		case bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp,
			bytecode.OpNewObject, bytecode.OpInstanceOf, bytecode.OpGetGlobal,
			bytecode.OpSetGlobal, bytecode.OpCheckGlobalRef, bytecode.OpSetGlobalStrict,
			bytecode.OpNewArray:
			if in.B != 0 {
				in.B += n
			}
		}
	}
	for i := range fn.LiteralKeys {
		fn.LiteralKeys[i].Site += n
	}
	fn.PropSites += n
}

// trace prints why a function has no register code (QJS_REGTRACE).
var trace = os.Getenv("QJS_REGTRACE") != ""

type gen struct {
	lit *ast.FuncLit
	fn  *bytecode.Function
	rec *Record
	// orig is the function refusals are recorded for: fn, or the function
	// fn is a view of (Later).
	orig *bytecode.Function

	// shareSites says the function has more cache sites than 16 bits
	// number, and is compiled again with the sites of one key shared
	// (site16).
	shareSites bool
	shared     map[siteKey]uint16
	// ownSites says the stack form alone has more sites than 16 bits
	// number: the register form numbers its own from 1 (nsites of them),
	// and the stack form's move past them (moveStackSites).
	ownSites bool
	nsites   uint32
	// farJumps says a jump of a 16-bit offset did not reach: the function
	// is compiled again with each such jump far (jump).
	farJumps bool
	// nfar is how many bindings have far registers (farReg); farPCs is
	// each instruction whose BC is one's k, which finish makes its
	// register, past every near one. nearCap, where it is not 0, is how
	// many near registers the bindings may take (layout): the function
	// was compiled once and ran out of registers.
	nfar    int
	farPCs  []int
	nearCap int

	code []bytecode.RInstr
	pos  []int32
	cur  int32

	// slotReg is the register of each stack slot the function's bindings
	// are in, or -1.
	slotReg []int32
	// dead is the call whose value the expression being compiled leaves
	// unused but for its truth: a statement's, a condition's.
	dead *ast.Call
	// marks is the calls' marks, by pc (RegCode.Marks).
	marks map[int]uint8
	// reuses says the code has a call_reuse, whose frame's locals the
	// runtime finds by LocalReg.
	reuses bool
	inits  int
	// lexical marks the slots of let and const bindings; tdz those whose
	// dead zone some read checks or a closure may see.
	tdz []bool
	// lazyArgs says the arguments object is made only on demand: its
	// local's register is undefined until args_index makes it.
	lazyArgs bool
	// leafSites are the stack form's property sites a leaf's register code
	// has not taken yet (propSite).
	leafSites map[leafSiteKey][]uint32
	top       int
	max       int
	loops     []*loop
	label     labelSet
	// usingCaps are the registers of the scopes with using declarations
	// around the code, innermost last, and disposeCaps each scope's, by its
	// finally clause's statement (usingScope).
	usingCaps   []int
	disposeCaps map[*ast.DisposeStmt]int
	// withs is each name a with object or an eval's variable may answer,
	// whose record says ResWith (with.go), and withRefs the registers of
	// the object and the found flag a reference's read settled, which its
	// store uses.
	withs    map[*ast.Ident]*withName
	withRefs map[*ast.Ident]withRef

	consts   map[any]uint32
	names    map[string]uint32
	caps     [][]uint16
	siteName []uint32
	auxes    map[int]uint32
	literals [][]bytecode.LitElem
	objKeys  [][]uint32
	// farNames is the index of each name past 16 bits an instruction
	// names, by its pc (RNameFar).
	farNames map[int]uint32
	calls    int
	// arrays is how many empty array literals the code has, each a site
	// that learns its arrays' room.
	arrays int
	// stores is, for an instruction that reads and then stores, the
	// position of its store, whose stack instruction goes to Aux.
	stores map[int]int32
	// switches is each switch instruction's table, its cases' bodies by
	// their marks, bound by the end.
	switches [][]switchEntry
	// prots are the handlers in force where code is compiled, outermost
	// first; handlers the finished ones, innermost first; copies how deep
	// the finally clauses' copies being compiled nest (try.go).
	prots    []*prot
	handlers []*prot
	copies   int
	held     int
	err      string
	// completion is top-level code's completion value's register, or 0
	// (top.go).
	completion int
	// chain is the optional chain being compiled, or nil.
	chain *chainCtx
	// backLive is RegCode.BackLive.
	backLive []uint32
	// thisRef says this is read through the frame's binding
	// (RegCode.ThisRef); forwardsArgs that a super call hands on the
	// frame's arguments (RegCode.KeepsArgs).
	thisRef, forwardsArgs bool
	// thisBound says a derived constructor's super() has certainly bound
	// this, which is then register 1.
	thisBound bool
}

type loop struct {
	label     labelSet
	brk, cont *mark
	isLoop    bool
	// named says the statement is a labelled one that is no loop or
	// switch: only a break naming it leaves it.
	named        bool
	scopeTopMark int
}

// mark is a place in the code jumps go to, and the jumps waiting for it.
type mark struct {
	at   int
	refs []jumpRef
}

type jumpRef struct {
	at    int
	short bool
}

func (g *gen) fail(why string) {
	if g.err == "" {
		g.err = why
	}
}

// layout gives each binding of the function a register: the parameters
// theirs, and every other slot the record, a hoisted function or a closure
// names, in slot order.
func (g *gen) layout() bool {
	fn := g.fn
	n := fn.LocalCount
	g.slotReg = make([]int32, n)
	g.tdz = make([]bool, n)
	used := make([]bool, n)
	// The arguments object's local, if it is made only on demand, and
	// whether anything reads or writes it as a binding -- which the stack
	// form makes it for first (lazy_arguments); then it is made as the
	// function starts.
	lazySlot, plain := -1, make([]bool, n)
	note := func(res Res) bool {
		switch res.Kind {
		case ResLocal, ResLocalCheck, ResLocalInit, ResLazyArgs:
			if int(res.Index) >= n {
				return false
			}
			used[res.Index] = true
			if res.Kind == ResLazyArgs {
				lazySlot = int(res.Index)
			} else {
				plain[res.Index] = true
			}
			if res.Kind == ResLocalCheck {
				g.tdz[res.Index] = true
			}
		}
		return true
	}
	for _, res := range g.rec.Read {
		if !note(res) {
			return false
		}
	}
	for _, res := range g.rec.Write {
		if !note(res) {
			return false
		}
	}
	// A direct eval reaches a binding in scope by its slot (LocalReg),
	// whichever it names: each has a register, and the arguments object
	// is made as the function starts. So does a debugger, at any place
	// the code can stop: it shows the bindings in scope there, and
	// evaluates code there as a direct eval.
	evals := len(g.rec.Evals) > 0 || g.fn.Debug != nil
	if evals {
		for i := range used {
			used[i] = true
		}
	}
	g.lazyArgs = lazySlot >= 0 && !plain[lazySlot] && !evals
	defer g.probedNames()
	if c := g.rec.Completion; c >= 0 {
		if int(c) >= n {
			return false
		}
		used[c] = true
	}
	for _, slot := range g.rec.Funcs {
		if int(slot) >= n {
			return false
		}
		used[slot] = true
	}
	// A class's scope: its inner name, its private names' keys, its list
	// of private methods and its computed keys, each a register.
	mark := func(slot int32) bool {
		if slot < 0 {
			return true
		}
		if int(slot) >= n {
			return false
		}
		used[slot] = true
		return true
	}
	for _, cr := range g.rec.Classes {
		ok := mark(cr.Self) && mark(cr.Install)
		for _, p := range cr.Privates {
			ok = ok && mark(int32(p.Slot))
		}
		for _, k := range cr.Keys {
			ok = ok && mark(k)
		}
		if !ok {
			return false
		}
	}
	for _, res := range g.rec.Privates {
		if res.Kind == ResLocal && !mark(int32(res.Index)) {
			return false
		}
	}
	for _, c := range fn.Constants {
		if c.Kind != bytecode.ConstFunction || c.Fn == nil {
			continue
		}
		for _, u := range c.Fn.Upvalues {
			if u.FromParent {
				if int(u.Index) >= n {
					return false
				}
				used[u.Index] = true
			}
		}
	}
	params := fn.ParamCount
	if fn.HasRest && params < n {
		// The rest parameter's slot, after the others, is where rest_param
		// makes its array (params): its register is kept for it whether or
		// not anything reads it, or the binding given it would be written.
		params++
	}
	next := regParams + params
	need := next
	for i := params; i < n; i++ {
		if used[i] {
			need++
		}
	}
	var far []bool
	if need >= limit16 || g.nearCap > 0 {
		far = g.farSlots(used, plain, params, lazySlot, evals)
	}
	for i := range g.slotReg {
		switch {
		case i < params:
			g.slotReg[i] = int32(regParams + i)
		case far != nil && far[i]:
			g.slotReg[i] = int32(farReg + g.nfar)
			g.nfar++
		case used[i]:
			g.slotReg[i] = int32(next)
			next++
		default:
			g.slotReg[i] = -1
		}
	}
	g.inits = next - regParams - fn.ParamCount
	if c := g.rec.Completion; c >= 0 {
		g.completion = int(g.slotReg[c])
	}
	g.top, g.max = next, next
	if next >= limit16 {
		return false
	}
	g.consts = map[any]uint32{}
	g.names = map[string]uint32{}
	for i, name := range fn.Names {
		if _, ok := g.names[name]; !ok {
			g.names[name] = uint32(i)
		}
	}
	g.caps = make([][]uint16, len(fn.Constants))
	if g.ownSites {
		g.siteName = make([]uint32, 1)
	} else {
		g.siteName = make([]uint32, fn.PropSites+1)
	}
	return true
}

// farSlots is, for a function whose bindings do not fit 16 bits of
// registers (or did not leave its temporaries room, nearCap), which of them
// have far registers, or nil where none can -- too many must be near, or
// all fit. A binding only
// plain reads and writes reach can be far -- one no closure captures, no
// read finds in its dead zone, no direct eval or debugger reaches, nor
// the arguments object, a catch clause's, a class's, a function
// declaration's or the completion value -- and of those the ones used
// least are, in slot order, until the rest fit.
func (g *gen) farSlots(used, plain []bool, params, lazySlot int, evals bool) []bool {
	if evals {
		return nil
	}
	fn := g.fn
	n := len(used)
	can := make([]bool, n)
	for i := params; i < n; i++ {
		can[i] = used[i] && plain[i] && !g.tdz[i] && i != lazySlot && !fn.Locals[i].Captured && fn.Locals[i].Name != "arguments"
	}
	near := func(slot int64) {
		if slot >= 0 && slot < int64(n) {
			can[slot] = false
		}
	}
	near(int64(g.rec.Completion))
	for _, slot := range g.rec.Funcs {
		near(int64(slot))
	}
	for _, cr := range g.rec.Classes {
		near(int64(cr.Self))
		near(int64(cr.Install))
		for _, p := range cr.Privates {
			near(int64(p.Slot))
		}
		for _, k := range cr.Keys {
			near(int64(k))
		}
	}
	for _, res := range g.rec.Privates {
		near(int64(res.Index))
	}
	for _, slot := range g.rec.Catches {
		near(int64(slot))
	}
	uses := make([]int, n)
	for _, res := range g.rec.Read {
		if int(res.Index) < n {
			uses[res.Index]++
		}
	}
	for _, res := range g.rec.Write {
		if int(res.Index) < n {
			uses[res.Index]++
		}
	}
	room := g.nearCap
	if room == 0 {
		// A quarter of the registers left for the temporaries.
		room = limit16 * 3 / 4
	}
	room -= regParams + params
	var cands []int
	for i := params; i < n; i++ {
		switch {
		case can[i]:
			cands = append(cands, i)
		case used[i]:
			room--
		}
	}
	if room < 0 {
		return nil
	}
	if len(cands) <= room {
		// All fit: nearCap asked for room the bindings do not take.
		return nil
	}
	sort.SliceStable(cands, func(a, b int) bool { return uses[cands[a]] > uses[cands[b]] })
	far := make([]bool, n)
	for _, i := range cands[room:] {
		far[i] = true
	}
	return far
}

// --- emission -------------------------------------------------------------

func (g *gen) emit(w bytecode.RInstr) int {
	g.code = append(g.code, w)
	g.pos = append(g.pos, g.cur)
	return len(g.code) - 1
}

func (g *gen) at(pos int) { g.cur = int32(pos) }

// r16 is register r as an operand. A far binding's register is never
// one: every site that reads or writes a binding copies a far one through
// a near temporary (movFar, movToFar), so one here is regcomp's bug.
func r16(r int) uint16 {
	if r >= farReg {
		panic("regcomp: a far register as an operand")
	}
	return uint16(r)
}

// farOf is the k of slot's binding where it has a far register, and -1
// where it has a near one or none.
func (g *gen) farOf(slot uint32) int {
	if int(slot) < len(g.slotReg) && g.slotReg[slot] >= farReg {
		return int(g.slotReg[slot]) - farReg
	}
	return -1
}

// checkRef throws if register r, slot's binding, is in its dead zone
// (check_ref): B the slot, for the error's name, or RNameFar past 16 bits
// and FarNames the slot.
func (g *gen) checkRef(r int, slot uint32) {
	if slot < bytecode.RNameFar {
		g.emit(bytecode.MakeR(bytecode.RCheckRef, 0, r16(r), uint16(slot), 0))
		return
	}
	pc := g.emit(bytecode.MakeR(bytecode.RCheckRef, 0, r16(r), bytecode.RNameFar, 0))
	if g.farNames == nil {
		g.farNames = map[int]uint32{}
	}
	g.farNames[pc] = slot
}

// movFar copies the k-th far binding to register d (mov_far).
func (g *gen) movFar(d, k int) {
	g.farPCs = append(g.farPCs, g.emit(bytecode.MakeRBC(bytecode.RMovFar, 0, r16(d), uint32(k))))
}

// movToFar copies register v to the k-th far binding (mov_to_far).
func (g *gen) movToFar(k, v int) {
	g.farPCs = append(g.farPCs, g.emit(bytecode.MakeRBC(bytecode.RMovToFar, 0, r16(v), uint32(k))))
}

func (g *gen) alloc() int {
	r := g.top
	g.top++
	if g.top > g.max {
		g.max = g.top
	}
	if g.top >= limit16 {
		g.fail(errRegs)
	}
	return r
}

func (g *gen) allocN(n int) int {
	r := g.top
	for i := 0; i < n; i++ {
		g.alloc()
	}
	return r
}

// freeAbove gives back the registers an expression took from mark on but
// its value's, d: a call's window and what its callee and arguments took,
// dead once the call is made. Kept until the statement's end, a chain of
// calls -- a.f(x).f(y).f(z)..., each call's receiver the one before it --
// took a window for each, the innermost the highest: minified bundles'
// chains of thousands of calls had a register for each instruction.
func (g *gen) freeAbove(mark, d int) int {
	if d >= mark {
		g.top = d + 1
	} else {
		g.top = mark
	}
	return d
}

// result is dst, or, for -1, the first temporary an operation's
// operands took from mark on, the rest given back: what an operation
// writes it writes after it has read its operands, so its value may be
// where one of them was. Each step of a long expression -- a determinant's
// products and sums -- had taken one of its own.
func (g *gen) result(mark, dst int) int {
	if dst >= 0 {
		return dst
	}
	g.top = mark
	return g.alloc()
}

// dest is dst, or a fresh temporary for -1.
func (g *gen) dest(dst int) int {
	if dst >= 0 {
		return dst
	}
	return g.alloc()
}

func (g *gen) newMark() *mark { return &mark{at: -1} }

func (g *gen) bind(m *mark) {
	m.at = len(g.code)
	for _, ref := range m.refs {
		g.patch(ref, m.at)
	}
	m.refs = nil
}

func (g *gen) patch(ref jumpRef, target int) {
	off := int64(target) - int64(ref.at+1)
	if ref.short {
		if off < -1<<15 || off >= 1<<15 {
			g.fail(errJumps)
			return
		}
		g.code[ref.at] = g.code[ref.at].WithSC(int32(off))
		return
	}
	g.code[ref.at] = g.code[ref.at].WithBC(uint32(int32(off)))
}

// jump emits w, a jump to m: short says its offset is SC, else BC.
func (g *gen) jump(w bytecode.RInstr, m *mark, short bool) {
	if short && g.farJumps {
		// A jump of a 16-bit offset in a function where one did not
		// reach: the branch taken to a jmp of 32 bits, past a jmp over it
		// where it is not.
		g.emit(w.WithSC(1))
		g.emit(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0).WithBC(1))
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), m, false)
		return
	}
	at := g.emit(w)
	ref := jumpRef{at: at, short: short}
	if m.at >= 0 {
		// A loop's backward jump: what is live at its head is the
		// bindings and the registers the statements around it hold.
		g.backLive = append(g.backLive, uint32(at), uint32(g.stmtTop()))
		g.patch(ref, m.at)
		return
	}
	m.refs = append(m.refs, ref)
}

func (g *gen) site(name uint32) uint32 {
	if g.ownSites {
		g.nsites++
		g.siteName = append(g.siteName, name)
		return g.nsites
	}
	g.fn.PropSites++
	s := g.fn.PropSites
	g.siteName = append(g.siteName, 0)
	g.siteName[s] = name
	return s
}

// errSites is the failure of a site past 16 bits, which Compile meets
// by compiling the function again with its sites shared (gen.shareSites).
const errSites = "a site past 16 bits"

// lazySites is the number of sites in a function's stack form from which
// Later compiles its register form at once: half of what 16 bits number,
// which leaves a function compiled at its first call the other half for
// its register form's shared sites.
const lazySites = 1 << 15

// errJumps is the failure of a jump past a 16-bit offset, which Compile
// meets by compiling the function again with its jumps far (farJumps).
const errJumps = "a jump past 16 bits"

// siteKey is what a site an instruction's 16-bit operand names is shared
// by, where a function has more sites than 16 bits number: the kind of
// cache, the name it answers for, and for a literal its keys or its
// constants. Two instructions of one key keep the same entries -- a
// shape's answer for that kind and name, a literal's room and final shape
// for its keys, its constants' array -- so that one site serves both, as
// a polymorphic one.
type siteKey struct {
	kind string
	name uint32
	keys string
}

// site16 is a cache site for an instruction's 16-bit operand: one of its
// own, or in a function compiled with its sites shared, the one of its
// key.
func (g *gen) site16(k siteKey) uint16 {
	if g.shareSites {
		if s, ok := g.shared[k]; ok {
			return s
		}
	}
	s := g.site(k.name)
	if s >= limit16 {
		g.fail(errSites)
		return 0
	}
	if g.shareSites {
		if g.shared == nil {
			g.shared = map[siteKey]uint16{}
		}
		g.shared[k] = uint16(s)
	}
	return uint16(s)
}

// stackSite is the cache site of the stack form's property read in, a
// get_prop whose A is its name and B its site, for a read of the register
// form that shares it.
func (g *gen) stackSite(in bytecode.Instr) uint16 {
	if g.ownSites {
		// The stack form's sites move past the register form's.
		return g.propSite(bytecode.OpGetProp, in.A)
	}
	if in.B == 0 || in.B >= uint32(len(g.siteName)) || in.A >= uint32(len(g.fn.Names)) {
		g.fail("a stack site out of range")
		return 0
	}
	if in.B >= limit16 {
		// Past 16 bits: a site of the register form's.
		return g.propSite(bytecode.OpGetProp, in.A)
	}
	g.siteName[in.B] = in.A
	return uint16(in.B)
}

// propSite is the cache site of a read (get_prop), a method's read
// (get_prop_this) or a store (set_prop), op, of the name with index name.
// In a leaf -- a body the frameless evaluators run from the stack form,
// with the stack form's sites -- it is the next site of the stack form's
// of that kind and name not yet taken, so that a run of the register code
// fills the caches those evaluators read: an entry is the shape's answer
// for the name and kind, whichever site of them holds it. Anywhere else,
// and past the stack form's sites, it is a site of its own.
func (g *gen) propSite(op bytecode.Op, name uint32) uint16 {
	if g.fn.Leaf == bytecode.LeafNone || g.ownSites {
		return g.site16(siteKey{kind: op.String(), name: name})
	}
	if g.leafSites == nil {
		g.leafSites = map[leafSiteKey][]uint32{}
		for _, in := range g.fn.Code {
			switch in.Op {
			case bytecode.OpGetProp, bytecode.OpGetPropThis, bytecode.OpSetProp:
				if in.B != 0 && in.B < uint32(len(g.siteName)) && in.A < uint32(len(g.fn.Names)) {
					k := leafSiteKey{in.Op, g.fn.Names[in.A]}
					g.leafSites[k] = append(g.leafSites[k], in.B)
				}
			}
		}
	}
	k := leafSiteKey{op, g.fn.Names[name]}
	q := g.leafSites[k]
	if len(q) == 0 || q[0] >= limit16 {
		return g.site16(siteKey{kind: op.String(), name: name})
	}
	g.leafSites[k] = q[1:]
	g.siteName[q[0]] = name
	return uint16(q[0])
}

// leafSiteKey is a kind of property access and its name, which propSite
// pairs the two forms' sites by.
type leafSiteKey struct {
	op   bytecode.Op
	name string
}

func (g *gen) nameIdx(name string) uint32 {
	if i, ok := g.names[name]; ok {
		return i
	}
	i := uint32(len(g.fn.Names))
	g.fn.Names = append(g.fn.Names, name)
	g.names[name] = i
	return i
}

// name16 is name's operand: its index in the function's names, or
// RNameFar past 16 bits, where the instruction that has it is given the
// index by far.
func (g *gen) name16(name string) uint16 {
	i := g.nameIdx(name)
	if i >= bytecode.RNameFar {
		return bytecode.RNameFar
	}
	return uint16(i)
}

// far gives the instruction at pc the index of name, its operand
// name16's, where that is past 16 bits (RegCode.FarNames). It returns pc.
func (g *gen) far(pc int, name string) int {
	if i := g.nameIdx(name); i >= bytecode.RNameFar {
		if g.farNames == nil {
			g.farNames = map[int]uint32{}
		}
		g.farNames[pc] = i
	}
	return pc
}

func (g *gen) constIdx(c bytecode.Constant) uint32 {
	var key any
	switch c.Kind {
	case bytecode.ConstNumber:
		key = math.Float64bits(c.Num)
	case bytecode.ConstString:
		key = c.Str
	default:
		key = len(g.fn.Constants)
	}
	if i, ok := g.consts[key]; ok {
		return i
	}
	i := uint32(len(g.fn.Constants))
	g.fn.Constants = append(g.fn.Constants, c)
	g.caps = append(g.caps, nil)
	g.consts[key] = i
	return i
}

// --- functions and statements ---------------------------------------------

func (g *gen) body() {
	lit := g.lit
	// A list of parameters with a default, a pattern or a rest binds them
	// one at a time: those a read checks or a closure may see are taken
	// aside and put in their dead zone below, until their turn.
	var args []int
	lexical := !g.fn.HasSimpleParams || g.fn.HasRest
	if suspends(g.fn) {
		// A resumption goes on from here where the frame was suspended.
		g.emit(bytecode.MakeR(bytecode.RGenEntry, 0, 0, 0, 0))
	}
	if g.nfar > 0 {
		// The far bindings start undefined, as the near ones the call
		// makes (Inits).
		at := g.emit(bytecode.MakeRBC(bytecode.RFarInit, 0, 0, 0))
		g.farPCs = append(g.farPCs, at)
		g.aux(at, uint32(g.nfar))
	}
	if lexical {
		args = g.paramsAside()
	}
	release := g.hold()
	// The bindings whose dead zone a read checks or a closure may see start
	// in it; a self name a closure captures is the function.
	for slot, need := range g.tdz {
		if need && g.slotReg[slot] >= 0 {
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(int(g.slotReg[slot])), 0, 0))
		}
	}
	if g.completion > 0 {
		release()
		g.topBody()
		// Strict eval code's function declarations are its own bindings,
		// made before its first statement as a function's are.
		for _, s := range lit.Body {
			if fd := hoistable(s); fd != nil {
				if slot, ok := g.rec.Funcs[fd]; ok {
					g.closure(fd.Fn, int(g.slotReg[slot]))
				}
			}
		}
		g.stmts(lit.Body)
		g.retReg(g.completion)
		return
	}
	if lit.Name != nil && !declares(lit.Body, lit.Name.Name) {
		for slot, l := range g.fn.Locals {
			if slot < len(g.slotReg) && g.slotReg[slot] >= 0 && l.Name == lit.Name.Name && !g.declared(slot) {
				if k := g.farOf(uint32(slot)); k >= 0 {
					g.movToFar(k, regCallee)
				} else {
					g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(int(g.slotReg[slot])), regCallee, 0))
				}
			}
		}
	}
	// The arguments object, made as the function starts, in its binding.
	if (g.fn.UsesArguments || g.fn.MakesArguments) && !g.fn.ForwardsArguments && !g.lazyArgs {
		named := false
		for slot, l := range g.fn.Locals {
			if l.Name != "arguments" || slot >= len(g.slotReg) {
				continue
			}
			named = true
			// A binding nothing reads has no register, and no object is
			// made for it: a function that names eval without calling it
			// has one, which the stack form fills for an eval that never
			// comes.
			if g.slotReg[slot] >= 0 {
				g.emit(bytecode.MakeR(bytecode.RArguments, 0, r16(int(g.slotReg[slot])), 0, 0))
			}
		}
		if !named {
			g.fail("an arguments binding the record does not name")
			return
		}
	}
	if lexical {
		g.params(args)
	}
	release()
	g.top = g.topAfterBindings()
	if g.fn.Generator {
		// A generator's creation binds its parameters and stops here.
		g.cold(bytecode.REndParams, 0, 0, 0)
	}
	// Hoisted function declarations of the body; a block's are made as the
	// block is entered (enterBlock).
	for _, s := range lit.Body {
		if fd := hoistable(s); fd != nil {
			slot, ok := g.rec.Funcs[fd]
			if !ok {
				g.fail("a function declaration without a binding")
				return
			}
			g.closure(fd.Fn, int(g.slotReg[slot]))
		}
	}
	if g.fn.Kind != bytecode.KindDerivedConstructor {
		g.stmts(lit.Body)
		g.retUndef()
		return
	}
	// A derived constructor's statements: once one of them that always
	// runs is a super() call, this is bound for good, and in register 1,
	// where super_call put it.
	g.enterBlock(lit.Body)
	run := func() {
		for _, s := range lit.Body {
			if g.err != "" {
				return
			}
			g.stmt(s)
			if es, ok := s.(*ast.ExprStmt); ok {
				if c, ok := es.X.(*ast.Call); ok && isSuper(c.Callee) {
					g.thisBound = true
				}
			}
		}
	}
	if has, async := usingKind(lit.Body); has {
		g.usingScope(lit.Body[0].Pos(), async, run)
	} else {
		run()
	}
	g.retUndef()
}

// simpleArguments reports whether fn's use of arguments is the one the
// backend compiles: an object made as the function starts, in the binding
// the stack form gives it, of a function with simple parameters.
func simpleArguments(fn *bytecode.Function) bool {
	// A function whose arguments object is only handed on to apply
	// (ForwardsArguments) has no object made: each such call is an
	// apply_args, which hands on the frame's arguments as passed.
	if fn.TopLevel {
		return false
	}
	switch fn.Kind {
	case bytecode.KindNormal, bytecode.KindMethod, bytecode.KindGetter, bytecode.KindSetter,
		bytecode.KindConstructor, bytecode.KindDerivedConstructor:
	default:
		return false
	}
	for _, l := range fn.Locals {
		if l.Name == "arguments" {
			return true
		}
	}
	return false
}

// switchStmt compiles a switch: the discriminant once, each case's test
// with ===, in order, jumping to its body, and default's body where no test
// matched; the bodies in order, each falling into the next.
func (g *gen) switchStmt(n *ast.SwitchStmt, label labelSet) {
	var all []ast.Stmt
	for _, c := range n.Cases {
		all = append(all, c.Body...)
	}
	// switch (typeof x) of string cases alone looks x's type up, with no
	// string made or compared.
	op := bytecode.RSwitch
	disc := n.Disc
	if u, ok := n.Disc.(*ast.Unary); ok && u.Op == "typeof" && g.stringCases(n) {
		global := false
		if id, ok := u.Operand.(*ast.Ident); ok {
			res := g.rec.Read[id]
			global = res.Kind == ResGlobal || res.Kind == ResNone
		}
		if !global {
			op, disc = bytecode.RSwitchTypeof, u.Operand
		}
	}
	d := g.expr(disc, -1)
	if g.isBinding(d) {
		t := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(t), r16(d), 0))
		d = t
	}
	// The case block is one scope, entered once the discriminant is made.
	g.enterBlock(all)
	l := &loop{label: label, brk: g.newMark()}
	if len(label) > 0 {
		l.cont = nil
	}
	g.loops = append(g.loops, l)
	bodies := make([]*mark, len(n.Cases))
	def := -1
	for i := range n.Cases {
		bodies[i] = g.newMark()
	}
	// A run of cases whose selectors are literals, which evaluating does
	// nothing, is one lookup: strings and numbers compared by === one at
	// a time each go to the slow path or cost a dispatch. A case of
	// another selector is compared where it is, between the runs.
	compare := func(i int) {
		mark := g.top
		if imm, ok := imm16(n.Cases[i].Test); ok {
			g.jump(bytecode.MakeR(bytecode.RJSEqI, 0, r16(d), uint16(int16(imm)), 0), bodies[i], true)
		} else {
			v := g.expr(n.Cases[i].Test, -1)
			g.jump(bytecode.MakeR(bytecode.RJSEq, 0, r16(d), r16(v), 0), bodies[i], true)
		}
		g.top = mark
	}
	var table []switchEntry
	var run []int
	strs := false
	flush := func() {
		if len(table) >= 2 || strs {
			g.emit(bytecode.MakeRBC(op, 0, r16(d), uint32(len(g.switches))))
			g.switches = append(g.switches, table)
		} else {
			for _, i := range run {
				compare(i)
			}
		}
		table, run, strs = nil, nil, false
	}
	for i, c := range n.Cases {
		if c.Test == nil {
			def = i
			continue
		}
		if e, ok := g.switchLit(c.Test); ok {
			e.body = bodies[i]
			table = append(table, e)
			run = append(run, i)
			strs = strs || e.c.Kind == bytecode.SwitchStr
			continue
		}
		flush()
		compare(i)
	}
	flush()
	if def >= 0 {
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), bodies[def], false)
	} else {
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), l.brk, false)
	}
	for i, c := range n.Cases {
		g.bind(bodies[i])
		for _, s := range c.Body {
			if g.err != "" {
				return
			}
			g.stmt(s)
		}
	}
	g.bind(l.brk)
	g.loops = g.loops[:len(g.loops)-1]
}

// typeNames are the VM's typeofNames, in its order.
var typeNames = [8]string{"undefined", "object", "boolean", "number", "string", "symbol", "bigint", "function"}

// typeofCompare is x and the type's index where n compares typeof x with a
// string literal that names a type, by ===, ==, !== or !=, x not a global's
// name, whose typeof the backend leaves to the stack form.
func (g *gen) typeofCompare(n *ast.Binary) (ast.Expr, int, bool) {
	switch n.Op {
	case "===", "==", "!==", "!=":
	default:
		return nil, 0, false
	}
	u, isU := n.Left.(*ast.Unary)
	s, isS := n.Right.(*ast.StringLit)
	if !isU || !isS {
		u, isU = n.Right.(*ast.Unary)
		s, isS = n.Left.(*ast.StringLit)
	}
	if !isU || !isS || u.Op != "typeof" {
		return nil, 0, false
	}
	for i, name := range typeNames {
		if name == s.Value {
			return u.Operand, i, true
		}
	}
	return nil, 0, false
}

// stringCases reports whether every case of n but the default has a
// string literal for its selector.
func (g *gen) stringCases(n *ast.SwitchStmt) bool {
	for _, c := range n.Cases {
		if _, ok := c.Test.(*ast.StringLit); c.Test != nil && !ok {
			return false
		}
	}
	return true
}

// switchEntry is a case of a switch instruction's table, its body's pc
// the mark's.
type switchEntry struct {
	c    bytecode.SwitchCase
	body *mark
}

// switchLit is the table's case of a selector that is a literal.
func (g *gen) switchLit(e ast.Expr) (switchEntry, bool) {
	if v, ok := numberLit(e); ok {
		return switchEntry{c: bytecode.SwitchCase{Kind: bytecode.SwitchNum, Num: v}}, true
	}
	switch n := e.(type) {
	case *ast.StringLit:
		k := g.constIdx(bytecode.Constant{Kind: bytecode.ConstString, Str: n.Value})
		return switchEntry{c: bytecode.SwitchCase{Kind: bytecode.SwitchStr, Str: n.Value, K: k}}, true
	case *ast.BoolLit:
		if n.Value {
			return switchEntry{c: bytecode.SwitchCase{Kind: bytecode.SwitchTrue}}, true
		}
		return switchEntry{c: bytecode.SwitchCase{Kind: bytecode.SwitchFalse}}, true
	case *ast.NullLit:
		return switchEntry{c: bytecode.SwitchCase{Kind: bytecode.SwitchNull}}, true
	}
	return switchEntry{}, false
}

// declares reports whether the statements of a function's body declare
// name, with var or a function declaration anywhere but in a nested
// function, or with let or const in the body itself: what shadows a
// function expression's own name. A let or const of a nested block shadows
// it only there, in a binding of its own.
func declares(list []ast.Stmt, name string) bool { return declaresIn(list, name, true) }

func declaresIn(list []ast.Stmt, name string, body bool) bool {
	for _, s := range list {
		switch n := s.(type) {
		case *ast.VarDecl:
			if !body && n.Kind != ast.DeclVar {
				continue
			}
			for _, d := range n.Decls {
				if id, ok := d.Target.(*ast.Ident); !ok || id.Name == name {
					return true
				}
			}
		case *ast.FuncDecl:
			if n.Fn.Name != nil && n.Fn.Name.Name == name {
				return true
			}
		case *ast.BlockStmt:
			if declaresIn(n.Body, name, false) {
				return true
			}
		case *ast.IfStmt:
			if declaresIn([]ast.Stmt{n.Cons}, name, false) || n.Alt != nil && declaresIn([]ast.Stmt{n.Alt}, name, false) {
				return true
			}
		case *ast.ForStmt:
			if n.Init != nil && declaresIn([]ast.Stmt{n.Init}, name, false) || declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.WhileStmt:
			if declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.DoWhileStmt:
			if declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.LabeledStmt:
			if declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.SwitchStmt:
			for _, c := range n.Cases {
				if declaresIn(c.Body, name, false) {
					return true
				}
			}
		case *ast.WithStmt:
			if declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.TryStmt:
			if declaresIn(n.Block, name, false) || n.Catch != nil && declaresIn(n.Catch.Body, name, false) ||
				declaresIn(n.Finally, name, false) {
				return true
			}
		case *ast.ForOfStmt:
			if s, ok := n.Left.(ast.Stmt); ok && declaresIn([]ast.Stmt{s}, name, false) || declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.ForInStmt:
			if s, ok := n.Left.(ast.Stmt); ok && declaresIn([]ast.Stmt{s}, name, false) || declaresIn([]ast.Stmt{n.Body}, name, false) {
				return true
			}
		case *ast.ExprStmt, *ast.ReturnStmt, *ast.EmptyStmt, *ast.BreakStmt, *ast.ContinueStmt, *ast.ThrowStmt:
		default:
			// A statement the backend does not compile anyway.
			return true
		}
	}
	return false
}

// declared reports whether slot is a binding the record says the body
// stores to or reads, which a self name is not.
func (g *gen) declared(slot int) bool {
	for _, res := range g.rec.Write {
		if (res.Kind == ResLocal || res.Kind == ResLocalInit || res.Kind == ResLocalCheck) && int(res.Index) == slot {
			return true
		}
	}
	for _, s := range g.rec.Funcs {
		if int(s) == slot {
			return true
		}
	}
	return slot < g.fn.ParamCount
}

func (g *gen) stmts(list []ast.Stmt) {
	g.enterBlock(list)
	run := func() {
		for _, s := range list {
			if g.err != "" {
				return
			}
			g.stmt(s)
		}
	}
	if has, async := usingKind(list); has {
		// The scope's resources are disposed of however it is left.
		g.usingScope(list[0].Pos(), async, run)
		return
	}
	run()
}

// enterBlock puts the lexical bindings a block declares that start in their
// dead zone back in it, as each entry of the block makes them anew.
func (g *gen) enterBlock(list []ast.Stmt) {
	for _, s := range list {
		vd, ok := s.(*ast.VarDecl)
		if !ok || vd.Kind == ast.DeclVar {
			continue
		}
		var ids []*ast.Ident
		for _, d := range vd.Decls {
			ids = patternNames(d.Target, ids)
		}
		for _, id := range ids {
			res := g.rec.Write[id]
			if res.Kind != ResLocalInit {
				continue
			}
			slot := int(res.Index)
			if g.inLoop() && g.fn.Locals[slot].Captured {
				// Each turn's binding is a new one a closure keeps: the
				// closures of the turns before keep theirs, closed.
				g.cold(bytecode.RCloseRegs, int(g.slotReg[slot]), 1, 0)
			}
			if g.tdz[slot] || g.fn.Locals[slot].Captured && !(g.fn.Debug != nil && g.rec.Unmarked[uint32(slot)]) {
				// A closure may read it before its declaration runs. In a
				// debugger's code, which captures every binding, a closure
				// may where the stack form says one may (Record.Unmarked).
				g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(int(g.slotReg[slot])), 0, 0))
			}
		}
	}
	if len(list) == 0 || len(g.lit.Body) > 0 && &list[0] == &g.lit.Body[0] {
		// The body's own functions are made before its first statement.
		return
	}
	// A block's function declarations, made as it is entered, after its
	// lexical bindings, as the stack form's hoistBlockDeclarations makes
	// them. In a loop each entry's bindings are new ones: the captures of
	// the turn before are closed first, all of them, since one function
	// may capture another's binding, which is made after it.
	var funcs []*ast.FuncDecl
	for _, s := range list {
		fd := hoistable(s)
		if fd == nil {
			continue
		}
		slot, ok := g.rec.Funcs[fd]
		if !ok {
			g.fail("a function declaration without a binding")
			return
		}
		if g.inLoop() && g.fn.Locals[slot].Captured {
			g.cold(bytecode.RCloseRegs, int(g.slotReg[slot]), 1, 0)
		}
		funcs = append(funcs, fd)
	}
	for _, fd := range funcs {
		r := int(g.slotReg[g.rec.Funcs[fd]])
		g.closure(fd.Fn, r)
	}
}

// hoistable is the function declaration s is, through its labels, or nil:
// the stack form's hoistableFunction but for an export.
func hoistable(s ast.Stmt) *ast.FuncDecl {
	for {
		switch n := s.(type) {
		case *ast.FuncDecl:
			if n.Fn.Name != nil {
				return n
			}
			return nil
		case *ast.LabeledStmt:
			s = n.Body
		default:
			return nil
		}
	}
}

func (g *gen) inLoop() bool {
	for _, l := range g.loops {
		if l.isLoop {
			return true
		}
	}
	return false
}

func (g *gen) stmt(s ast.Stmt) {
	mark := g.top
	defer func() { g.top = mark }()
	label := g.label
	g.label = nil
	g.debugMark(s.Pos())
	switch n := s.(type) {
	case *ast.ExprStmt:
		g.exprStmt(n.X)
	case *ast.EmptyStmt, *ast.DebuggerStmt:
	case *ast.BlockStmt:
		g.stmts(n.Body)
	case *ast.VarDecl:
		g.varDecl(n)
	case *ast.FuncDecl:
		// Made as its list is entered. Annex B: a sloppy block's function
		// is also assigned to its var as the declaration is evaluated, what
		// the block's binding holds then, where the stack form recorded
		// the var's store (emitAnnexBFunctionAlias).
		if _, ok := g.rec.Write[n.Fn.Name]; ok {
			g.storeIdent(n.Fn.Name, n.Fn.Name, false)
		}
	case *ast.ReturnStmt:
		if g.fn.Async && g.fn.Generator && n.Arg != nil {
			// An async generator awaits what it returns.
			v := g.expr(n.Arg, -1)
			g.retFrom(g.awaitReg(v, n.Start))
			return
		}
		if g.finallyAround() {
			// The value is taken before the finally clauses run, which
			// may assign what it was read from.
			v := -1
			if n.Arg != nil {
				v = g.alloc()
				g.into(n.Arg, v)
			}
			defer g.hold()()
			g.leave(0, func() {
				if v < 0 {
					g.retUndef()
				} else {
					g.retReg(v)
				}
			})
			return
		}
		if n.Arg == nil {
			g.retUndef()
			return
		}
		g.ret(n.Arg)
	case *ast.IfStmt:
		g.resetCompletion()
		els := g.newMark()
		g.cond(n.Test, false, els)
		g.stmt(n.Cons)
		if n.Alt == nil {
			g.bind(els)
			return
		}
		end := g.newMark()
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
		g.bind(els)
		g.stmt(n.Alt)
		g.bind(end)
	case *ast.ForStmt:
		g.resetCompletion()
		g.forStmt(n, label)
	case *ast.WhileStmt:
		g.resetCompletion()
		l := g.pushLoop(label)
		test := g.newMark()
		body := g.newMark()
		l.cont = test
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), test, false)
		g.bind(body)
		g.stmt(n.Body)
		g.bind(test)
		g.debugMark(n.Test.Pos())
		g.cond(n.Test, true, body)
		g.bind(l.brk)
		g.popLoop()
	case *ast.DoWhileStmt:
		g.resetCompletion()
		l := g.pushLoop(label)
		body := g.newMark()
		g.bind(body)
		g.stmt(n.Body)
		g.bind(l.cont)
		g.debugMark(n.Test.Pos())
		g.cond(n.Test, true, body)
		g.bind(l.brk)
		g.popLoop()
	case *ast.BreakStmt:
		l := g.findLoop(n.Label, false)
		if l == nil {
			g.fail("a break without its target")
			return
		}
		g.leave(g.protsOutside(l, false), func() { g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), l.brk, false) })
	case *ast.ContinueStmt:
		l := g.findLoop(n.Label, true)
		if l == nil {
			g.fail("a continue without its loop")
			return
		}
		g.leave(g.protsOutside(l, true), func() { g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), l.cont, false) })
	case *ast.ThrowStmt:
		r := g.expr(n.Arg, -1)
		g.at(n.Start)
		if len(g.prots) > 0 {
			// The handler the table would find, entered as it would be:
			// nothing is made of the value that the handler would not
			// take straight back out, as the tree's throwToHandler does.
			p := g.prots[len(g.prots)-1]
			g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(p.reg), r16(r), 0))
			g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), p.to, false)
			return
		}
		g.emit(bytecode.MakeR(bytecode.RThrow, 0, r16(r), 0, 0))
	case *ast.SwitchStmt:
		g.resetCompletion()
		g.switchStmt(n, label)
	case *ast.TryStmt:
		g.resetCompletion()
		g.tryStmt(n)
	case *ast.WithStmt:
		g.withStmt(n)
	case *ast.ForOfStmt:
		g.resetCompletion()
		g.forOfStmt(n.Left, n.Right, n.Body, n.Await, false, label, n.Start)
	case *ast.ForInStmt:
		g.resetCompletion()
		g.forOfStmt(n.Left, n.Right, n.Body, false, true, label, n.Start)
	case *ast.ClassDecl:
		g.storeIdent(n.Class.Name, n.Class, true)
	case *ast.FieldInit:
		g.fieldInit(n)
	case *ast.InstallPrivateMethods:
		g.installPrivate(n)
	case *ast.ImportDecl:
		// The linker made its bindings.
	case *ast.ExportDecl:
		g.exportDecl(n)
	case *ast.DisposeStmt:
		g.disposeOut(n, -1)
	case *ast.LabeledStmt:
		labels := append(label[:len(label):len(label)], n.Label)
		switch n.Body.(type) {
		case *ast.ForStmt, *ast.WhileStmt, *ast.DoWhileStmt, *ast.ForOfStmt, *ast.ForInStmt,
			*ast.SwitchStmt, *ast.LabeledStmt:
			g.label = labels
			g.stmt(n.Body)
		default:
			// Any other statement: a break naming it goes to its end.
			l := &loop{label: labels, brk: g.newMark(), named: true}
			g.loops = append(g.loops, l)
			g.stmt(n.Body)
			g.popLoop()
			g.bind(l.brk)
		}
	default:
		g.fail(fmt.Sprintf("a statement the backend does not compile (%T)", s))
	}
}

// ret returns e: a conditional's each arm returns itself, with no move to
// a register the two share and no jump to the return.
func (g *gen) ret(e ast.Expr) {
	if c, ok := e.(*ast.Conditional); ok {
		els := g.newMark()
		g.cond(c.Test, false, els)
		mark := g.top
		g.ret(c.Cons)
		g.top = mark
		g.bind(els)
		g.ret(c.Alt)
		return
	}
	r := g.expr(e, -1)
	g.retReg(r)
}

func (g *gen) pushLoop(label labelSet) *loop {
	l := &loop{label: label, brk: g.newMark(), cont: g.newMark(), isLoop: true}
	g.loops = append(g.loops, l)
	return l
}

func (g *gen) popLoop() { g.loops = g.loops[:len(g.loops)-1] }

func (g *gen) findLoop(label string, cont bool) *loop {
	for i := len(g.loops) - 1; i >= 0; i-- {
		l := g.loops[i]
		if cont && !l.isLoop {
			// A switch or a labelled statement is no continue's target.
			if label != "" && l.label.has(label) {
				return nil
			}
			continue
		}
		if label == "" && !l.named || l.label.has(label) {
			return l
		}
	}
	return nil
}

// labelSet is the labels of a statement: `a: b: while (...)` is the
// target of a break or a continue naming either.
type labelSet []string

func (s labelSet) has(name string) bool {
	for _, x := range s {
		if x == name {
			return true
		}
	}
	return false
}

func (g *gen) forStmt(n *ast.ForStmt, label labelSet) {
	if vd, ok := n.Init.(*ast.VarDecl); ok && vd.Kind.IsUsing() {
		// What the head declares is disposed of when the loop is done
		// with, not at each turn.
		g.usingScope(n.Start, vd.Kind == ast.DeclAwaitUsing, func() { g.forLoop(n, label) })
		return
	}
	g.forLoop(n, label)
}

func (g *gen) forLoop(n *ast.ForStmt, label labelSet) {
	// The head's bindings are each turn's own where a closure captures
	// them; the check is made as the loop is entered.
	l := g.pushLoop(label)
	var copies []int
	if n.Init != nil {
		switch init := n.Init.(type) {
		case *ast.VarDecl:
			g.enterBlock([]ast.Stmt{init})
			g.varDecl(init)
			// A let binding a closure keeps is each turn's own, copied
			// from the turn before's (CreatePerIterationEnvironment):
			// closed after the head's initialization and at each turn's
			// end, the closures made before keep theirs.
			if init.Kind == ast.DeclLet {
				copies = g.capturedRegs(init)
				g.closeRegs(copies)
			}
		case *ast.ExprStmt:
			g.discard(init.X)
		default:
			g.fail("a for loop's head")
		}
	}
	g.top = g.stmtTop()
	test := g.newMark()
	body := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), test, false)
	g.bind(body)
	g.stmt(n.Body)
	g.bind(l.cont)
	g.closeRegs(copies)
	if n.Update != nil {
		mark := g.top
		g.debugMark(n.Update.Pos())
		g.discard(n.Update)
		g.top = mark
	}
	g.bind(test)
	if n.Test != nil {
		mark := g.top
		g.debugMark(n.Test.Pos())
		g.cond(n.Test, true, body)
		g.top = mark
	} else {
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), body, false)
	}
	g.bind(l.brk)
	g.popLoop()
}

// topAfterBindings is the first temporary register.
func (g *gen) topAfterBindings() int { return regParams + g.fn.ParamCount + g.inits }

// stmtTop is the first register a statement's temporaries may take: the
// first after the bindings and after the registers a statement around it
// holds for as long as it runs (hold) -- a for-of loop's cursor, a
// finally clause's exception, a return's value on its way through
// finally clauses.
func (g *gen) stmtTop() int { return max(g.topAfterBindings(), g.held) }

// hold keeps the registers below g.top from the temporaries of the
// statements compiled until the release it returns is called.
func (g *gen) hold() func() {
	saved := g.held
	g.held = g.top
	return func() { g.held = saved }
}

func (g *gen) varDecl(n *ast.VarDecl) {
	if n.Kind.IsUsing() {
		g.usingDecl(n)
		return
	}
	for _, d := range n.Decls {
		id, ok := d.Target.(*ast.Ident)
		if !ok {
			if d.Init == nil {
				// The parser allows one only in a for-of or for-in head,
				// which forOfStmt compiles.
				panic("regcomp: a pattern declared without a value")
			}
			g.pattern(d.Target, g.patternSource(d.Init), true)
			g.top = g.stmtTop()
			continue
		}
		if d.Init == nil {
			if n.Kind == ast.DeclVar {
				continue
			}
			res, ok := g.rec.Write[id]
			if ok && (res.Kind == ResGlobalLexInit || res.Kind == ResModuleLexInit) {
				v := g.alloc()
				g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(v), 0, 0))
				g.lexInit(res.Kind, id.Name, v, int(id.Start))
				g.top = g.stmtTop()
				continue
			}
			if !ok || res.Kind != ResLocalInit {
				g.fail("a declaration without a binding")
				return
			}
			if k := g.farOf(res.Index); k >= 0 {
				u := g.alloc()
				g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(u), 0, 0))
				g.movToFar(k, u)
				g.top = g.stmtTop()
				continue
			}
			g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(int(g.slotReg[res.Index])), 0, 0))
			continue
		}
		if fn, ok := d.Init.(*ast.FuncLit); ok && n.Kind == ast.DeclVar && g.rec.Reused[fn] && len(g.loops) > 0 {
			if res, ok := g.rec.Write[id]; ok && (res.Kind == ResLocal || res.Kind == ResLocalInit) && g.slotReg[res.Index] >= 0 && g.farOf(res.Index) < 0 {
				// A function nothing can tell from the one the last
				// iteration made, which the var holds then: made the
				// first time, and left after that, as the stack form
				// makes it.
				skip := g.newMark()
				g.jump(bytecode.MakeR(bytecode.RJmpT, 0, r16(int(g.slotReg[res.Index])), 0, 0), skip, false)
				g.storeIdent(id, d.Init, true)
				g.top = g.stmtTop()
				g.bind(skip)
				continue
			}
		}
		g.storeIdent(id, d.Init, true)
		g.top = g.stmtTop()
	}
}

// --- expressions ----------------------------------------------------------

// discard compiles e for its effects alone.
func (g *gen) discard(e ast.Expr) {
	switch n := e.(type) {
	case *ast.Update:
		if m, ok := n.Operand.(*ast.Member); ok {
			step := map[string]int32{"++": 1, "--": -1}[n.Op]
			if g.addProp(m, nil, step, n.Start) || g.addIndex(m, nil, step, n.Start) {
				return
			}
		}
		g.update(n, false, -1)
		return
	case *ast.Assign:
		if m, ok := n.Target.(*ast.Member); ok && n.Op == "+=" && (g.addProp(m, n.Value, 0, int(n.Start)) || g.addIndex(m, n.Value, 0, int(n.Start))) {
			return
		}
		g.assign(n, -1, false)
		return
	case *ast.Call:
		g.dead = n
	case *ast.Sequence:
		// Each expression's registers given back before the next, as a
		// statement's are: a minified bundle's comma sequences of
		// assignments, a statement each, took a register for each.
		mark := g.top
		for _, x := range n.Exprs {
			g.discard(x)
			g.top = mark
		}
		return
	case *ast.Logical:
		// A short circuit around an append whose value nothing uses, as
		// the stack form compiles it: the append stores, and leaves
		// nothing.
		if a, ok := n.Right.(*ast.Assign); ok && n.Op != "??" && g.rec.Appends[a] {
			skip := g.newMark()
			g.cond(n.Left, n.Op == "||", skip)
			g.discard(a)
			g.bind(skip)
			return
		}
	}
	g.expr(e, -1)
}

// isBinding reports whether r is a binding's register rather than a
// temporary.
func (g *gen) isBinding(r int) bool { return r >= regParams && r < g.topAfterBindings() }

// slotOf is the stack slot whose register is r, or -1.
func (g *gen) slotOf(r int) int {
	for slot, reg := range g.slotReg {
		if int(reg) == r {
			return slot
		}
	}
	return -1
}

// guard copies a binding's register r to a temporary where evaluating rest
// could change the binding before r is used: an assignment or an update of
// the binding in rest, or anything that runs code where a closure may
// write it.
func (g *gen) guard(r int, rest ast.Expr) int {
	if !g.isBinding(r) || rest == nil {
		return r
	}
	slot := g.slotOf(r)
	if slot < 0 {
		return r
	}
	// A parameter a sloppy arguments object aliases is written through it
	// by whatever the object is handed to, as a captured binding is.
	captured := g.fn.Locals[slot].Captured ||
		slot < g.fn.ParamCount && g.fn.MappedArguments && (g.fn.UsesArguments || g.fn.MakesArguments)
	if g.writes(rest, slot) || captured && !pure(rest) {
		t := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(t), r16(r), 0))
		return t
	}
	return r
}

// pure reports whether evaluating e runs no code of the program's.
func pure(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit, *ast.This:
		return true
	}
	_, ok := numberLit(e)
	return ok
}

// writes reports whether e assigns or updates the binding in slot.
func (g *gen) writes(e ast.Expr, slot int) bool {
	found := false
	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		if found || n == nil {
			return
		}
		switch x := n.(type) {
		case *ast.Assign:
			if id, ok := x.Target.(*ast.Ident); ok {
				if res, ok := g.rec.Write[id]; ok && (res.Kind == ResLocal || res.Kind == ResLocalInit || res.Kind == ResLocalCheck) && int(res.Index) == slot {
					found = true
					return
				}
			} else {
				walk(x.Target)
			}
			walk(x.Value)
		case *ast.Update:
			if id, ok := x.Operand.(*ast.Ident); ok {
				if res, ok := g.rec.Write[id]; ok && int(res.Index) == slot && res.Kind != ResGlobal {
					found = true
					return
				}
			}
			walk(x.Operand)
		case *ast.Binary:
			walk(x.Left)
			walk(x.Right)
		case *ast.Logical:
			walk(x.Left)
			walk(x.Right)
		case *ast.Unary:
			walk(x.Operand)
		case *ast.Conditional:
			walk(x.Test)
			walk(x.Cons)
			walk(x.Alt)
		case *ast.Call:
			walk(x.Callee)
			for _, a := range x.Args {
				walk(a)
			}
		case *ast.New:
			walk(x.Callee)
			for _, a := range x.Args {
				walk(a)
			}
		case *ast.Member:
			walk(x.Object)
			if x.Computed {
				walk(x.Property)
			}
		case *ast.Sequence:
			for _, y := range x.Exprs {
				walk(y)
			}
		case *ast.Yield:
			walk(x.Arg)
		case *ast.Await:
			walk(x.Arg)
		case *ast.ArrayLit:
			for _, y := range x.Elements {
				walk(y)
			}
		case *ast.ObjectLit:
			for _, p := range x.Props {
				walk(p.Value)
			}
		}
	}
	walk(e)
	return found
}

// direct reports whether e may be compiled into a binding's register
// itself: an operation that reads all its operands before it writes its
// result, rather than one that builds its result in the register first.
func direct(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.Assign:
		// An element's store whose value is made in the register last
		// (assign), after the store's object and key are read.
		t, ok := n.Target.(*ast.Member)
		return n.Op == "=" && ok && t.Computed && !t.Optional && direct(n.Value)
	case *ast.ObjectLit, *ast.ArrayLit, *ast.Logical, *ast.Conditional, *ast.Sequence, *ast.Update,
		*ast.ClassLit:
		return false
	}
	return true
}

// into compiles e into register dst.
func (g *gen) into(e ast.Expr, dst int) {
	if g.isBinding(dst) && !direct(e) {
		r := g.expr(e, -1)
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(dst), r16(r), 0))
		return
	}
	r := g.expr(e, dst)
	if r != dst {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(dst), r16(r), 0))
	}
}

// inPlace reports whether e is read where it is, in a binding's register
// or the this's, with no instruction but a check of its initialisation.
func (g *gen) inPlace(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.This:
		return !g.thisRef || g.thisBound
	case *regExpr:
		return true
	case *ast.Ident:
		res, ok := g.rec.Read[n]
		return ok && (res.Kind == ResLocal || res.Kind == ResLocalCheck) &&
			!(g.fn.ForwardsArguments && g.isArguments(n))
	}
	return false
}

// expr compiles e and returns the register holding its value: dst, or,
// for -1, wherever it is -- a binding's own register, read in place.
func (g *gen) expr(e ast.Expr, dst int) int {
	if g.err != "" {
		if dst >= 0 {
			return dst
		}
		return 0
	}
	switch n := e.(type) {
	case *ast.NumberLit:
		return g.number(n.Value, dst)
	case *ast.StringLit:
		d := g.dest(dst)
		if n.Value == "" {
			g.emit(bytecode.MakeR(bytecode.RLoadEmpty, 0, r16(d), 0, 0))
		} else {
			g.emit(bytecode.MakeRBC(bytecode.RLoadConst, 0, r16(d), g.constIdx(bytecode.Constant{Kind: bytecode.ConstString, Str: n.Value})))
		}
		return d
	case *ast.RegexpLit:
		d := g.dest(dst)
		g.at(n.Start)
		g.emit(bytecode.MakeRBC(bytecode.RNewRegExp, 0, r16(d), g.regexpConst(n)))
		return d
	case *ast.BoolLit:
		d := g.dest(dst)
		op := bytecode.RLoadFalse
		if n.Value {
			op = bytecode.RLoadTrue
		}
		g.emit(bytecode.MakeR(op, 0, r16(d), 0, 0))
		return d
	case *ast.NullLit:
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RLoadNull, 0, r16(d), 0, 0))
		return d
	case *ast.This:
		return g.thisExpr(n, dst)
	case *ast.ClassLit:
		return g.class(n, dst)
	case *ast.NewTarget:
		return g.newTarget(dst)
	case *ast.Ident:
		return g.ident(n, dst)
	case *ast.Binary:
		return g.binary(n, dst)
	case *ast.Logical:
		return g.logical(n, dst)
	case *ast.Unary:
		return g.unary(n, dst)
	case *ast.Update:
		return g.update(n, true, dst)
	case *ast.Assign:
		return g.assign(n, dst, true)
	case *ast.Conditional:
		d := g.dest(dst)
		els, end := g.newMark(), g.newMark()
		g.cond(n.Test, false, els)
		g.into(n.Cons, d)
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
		g.bind(els)
		g.into(n.Alt, d)
		g.bind(end)
		return d
	case *ast.Sequence:
		mark := g.top
		for _, x := range n.Exprs[:len(n.Exprs)-1] {
			g.discard(x)
			g.top = mark
		}
		return g.expr(n.Exprs[len(n.Exprs)-1], dst)
	case *ast.Member:
		return g.member(n, dst)
	case *ast.Call:
		mark := g.top
		return g.freeAbove(mark, g.call(n, dst))
	case *ast.New:
		mark := g.top
		return g.freeAbove(mark, g.newExpr(n, dst))
	case *ast.ObjectLit:
		return g.object(n, dst)
	case *ast.ArrayLit:
		return g.array(n, dst)
	case *ast.FuncLit:
		d := g.dest(dst)
		g.closure(n, d)
		return d
	case *ast.BigIntLit:
		return g.bigInt(n, dst)
	case *ast.TemplateLit:
		return g.template(n, dst)
	case *ast.TaggedTemplate:
		return g.taggedTemplate(n, dst)
	case *ast.OptionalChain:
		return g.optionalChain(n, dst)
	case *ast.Yield:
		return g.yieldExpr(n, dst)
	case *ast.Await:
		return g.awaitExpr(n, dst)
	case *regExpr:
		return g.place(n.reg, dst)
	case *ast.ImportMeta:
		d := g.dest(dst)
		g.cold(bytecode.RImportMeta, d, 0, 0)
		return d
	}
	g.fail(fmt.Sprintf("an expression the backend does not compile (%T)", e))
	return g.dest(dst)
}

// place is a value in register r, moved to dst if there is one.
func (g *gen) place(r, dst int) int {
	if dst < 0 || dst == r {
		return r
	}
	g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(dst), r16(r), 0))
	return dst
}

// isNullish reports whether e is the literal null.
func isNullish(e ast.Expr) bool {
	_, ok := e.(*ast.NullLit)
	return ok
}

func smallInt(v float64) (int32, bool) {
	i := int32(v)
	return i, float64(i) == v && !(v == 0 && math.Signbit(v))
}

// numberLit is the value of e where it is a number literal, or the
// negation of one: -1 is a unary minus of 1 in the AST, and an operand as
// much a constant as 1.
func numberLit(e ast.Expr) (float64, bool) {
	switch n := e.(type) {
	case *ast.NumberLit:
		return n.Value, true
	case *ast.Unary:
		if lit, ok := n.Operand.(*ast.NumberLit); ok && n.Op == "-" {
			return -lit.Value, true
		}
	}
	return 0, false
}

func imm16(e ast.Expr) (int32, bool) {
	n, ok := numberLit(e)
	if !ok {
		return 0, false
	}
	i, ok := smallInt(n)
	return i, ok && i >= -1<<15 && i < 1<<15
}

func (g *gen) number(v float64, dst int) int {
	d := g.dest(dst)
	if i, ok := smallInt(v); ok {
		g.emit(bytecode.MakeRBC(bytecode.RLoadInt, 0, r16(d), uint32(i)))
		return d
	}
	g.emit(bytecode.MakeRBC(bytecode.RLoadConst, 0, r16(d), g.constIdx(bytecode.Constant{Kind: bytecode.ConstNumber, Num: v})))
	return d
}

func (g *gen) ident(n *ast.Ident, dst int) int {
	res, ok := g.rec.Read[n]
	if !ok {
		g.fail("a name the record does not resolve")
		return g.dest(dst)
	}
	switch res.Kind {
	case ResWith:
		return g.withIdent(n, dst)
	case ResLocal, ResLocalCheck:
		if g.fn.ForwardsArguments && g.isArguments(n) {
			// Never made: only apply_args hands the arguments on.
			g.fail("the arguments object of a function that forwards it")
			return g.dest(dst)
		}
		if k := g.farOf(res.Index); k >= 0 {
			d := g.dest(dst)
			g.movFar(d, k)
			return d
		}
		r := int(g.slotReg[res.Index])
		if res.Kind == ResLocalCheck {
			g.at(int(n.Start))
			g.checkRef(r, res.Index)
		}
		g.freeze(n, r)
		return g.place(r, dst)
	case ResUpval:
		d := g.dest(dst)
		g.emit(bytecode.MakeRBC(bytecode.RGetUpval, 0, r16(d), res.Index))
		g.freeze(n, d)
		return d
	case ResUpvalCheck:
		d := g.dest(dst)
		g.at(int(n.Start))
		g.emit(bytecode.MakeRBC(bytecode.RGetUpvalCheck, 0, r16(d), res.Index))
		return d
	case ResGlobal:
		d := g.dest(dst)
		g.at(int(n.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RGetGlobal, 0, r16(d), g.name16(n.Name), g.globalSite("RGetGlobal", n.Name))), n.Name)
		return d
	case ResCallee:
		return g.place(regCallee, dst)
	case ResLiteral:
		if res.Lit == nil {
			d := g.dest(dst)
			g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(d), 0, 0))
			return d
		}
		return g.expr(res.Lit, dst)
	}
	g.fail("a name the backend does not read")
	return g.dest(dst)
}

// typeofOperand compiles typeof's operand e: a name that resolves nowhere,
// which the record does not resolve, as the stack form's get_global_opt,
// which does not throw for an undeclared global.
func (g *gen) typeofOperand(e ast.Expr) int {
	id, ok := e.(*ast.Ident)
	if !ok {
		return g.expr(e, -1)
	}
	if g.withs[id] != nil {
		g.probeOp(id, bytecode.OpWithTypeof)
		d := g.alloc()
		g.probed(id, bytecode.RWithGet, d, func() { g.place(g.typeofOperand(id), d) })
		return d
	}
	if _, ok := g.rec.Read[id]; ok {
		return g.expr(e, -1)
	}
	d := g.alloc()
	g.at(int(id.Start))
	g.far(g.emit(bytecode.MakeR(bytecode.RGetGlobalOpt, 0, r16(d), g.name16(id.Name), 0)), id.Name)
	return d
}

// globalSite is a cache site of a global's read or write, which keeps no
// name of the site table's.
func (g *gen) globalSite(kind string, name string) uint16 {
	return g.site16(siteKey{kind: kind, name: g.nameIdx(name)})
}

var binOps = map[string]bytecode.Op{
	"+": bytecode.OpAdd, "-": bytecode.OpSub, "*": bytecode.OpMul, "/": bytecode.OpDiv, "%": bytecode.OpMod,
	"**": bytecode.OpPow, "&": bytecode.OpBitAnd, "|": bytecode.OpBitOr, "^": bytecode.OpBitXor,
	"<<": bytecode.OpShl, ">>": bytecode.OpShr, ">>>": bytecode.OpUShr,
	"==": bytecode.OpEq, "!=": bytecode.OpNe, "===": bytecode.OpStrictEq, "!==": bytecode.OpStrictNe,
	"<": bytecode.OpLt, "<=": bytecode.OpLe, ">": bytecode.OpGt, ">=": bytecode.OpGe,
}

// regOps are the operators of two registers with an operation of their own.
var regOps = map[string]bytecode.ROp{
	"+": bytecode.RAdd, "-": bytecode.RSub, "*": bytecode.RMul,
	"&": bytecode.RBitAnd, "|": bytecode.RBitOr, "^": bytecode.RBitXor,
	"<<": bytecode.RShl, ">>": bytecode.RShr, ">>>": bytecode.RUShr,
}

// immOps are the operators of a register and an immediate with one.
var immOps = map[string]bytecode.ROp{
	"+": bytecode.RAddI, "-": bytecode.RSubI,
	"&": bytecode.RAndI, "|": bytecode.ROrI, "^": bytecode.RXorI,
	"<<": bytecode.RShlI, ">>": bytecode.RShrI, ">>>": bytecode.RUShrI,
}

func (g *gen) binary(n *ast.Binary, dst int) int {
	if n.Op == "+" {
		if d, ok := g.stringChain(n, dst); ok {
			return d
		}
	}
	switch n.Op {
	case "instanceof", "in":
		if pn, ok := n.Left.(*ast.PrivateName); ok {
			return g.privateIn(pn, n.Right, n.Start, dst)
		}
		a := g.guard(g.expr(n.Left, -1), n.Right)
		b := g.expr(n.Right, -1)
		d := g.dest(dst)
		g.at(n.Start)
		if n.Op == "in" {
			g.emit(bytecode.MakeR(bytecode.RIn, 0, r16(d), r16(a), r16(b)))
			return d
		}
		at := g.emit(bytecode.MakeR(bytecode.RInstanceOf, 0, r16(d), r16(a), r16(b)))
		g.aux(at, uint32(g.site(0)))
		return d
	}
	op, ok := binOps[n.Op]
	if !ok {
		g.fail("the operator " + n.Op)
		return g.dest(dst)
	}
	if _, isCmp := g.compareJump(n.Op, true); isCmp {
		// A comparison's value, through its compare-and-branch, whose fast
		// path the generic operator has not: load true, branch over the
		// load of false.
		d := g.dest(dst)
		if g.isBinding(d) {
			d = g.alloc()
		}
		done := g.newMark()
		yes := g.newMark()
		g.cond(n, true, yes)
		g.emit(bytecode.MakeR(bytecode.RLoadFalse, 0, r16(d), 0, 0))
		g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), done, false)
		g.bind(yes)
		g.emit(bytecode.MakeR(bytecode.RLoadTrue, 0, r16(d), 0, 0))
		g.bind(done)
		return g.place(d, dst)
	}
	return g.operate(n.Op, op, n.Left, n.Right, -1, dst, n.Start)
}

// operate is left op right into dst; a is left's register, where it has
// been evaluated already (a compound assignment's target), or -1.
func (g *gen) operate(text string, op bytecode.Op, left, right ast.Expr, a, dst, pos int) int {
	mark := g.top
	if a < 0 {
		a = g.guard(g.expr(left, -1), right)
	}
	if imm, ok := imm16(right); ok {
		if rop, ok := immOps[text]; ok {
			d := g.result(mark, dst)
			g.at(pos)
			g.emit(bytecode.MakeR(rop, 0, r16(d), r16(a), uint16(int16(imm))))
			return d
		}
	}
	b := g.expr(right, -1)
	d := g.result(mark, dst)
	g.at(pos)
	if rop, ok := regOps[text]; ok {
		g.emit(bytecode.MakeR(rop, 0, r16(d), r16(a), r16(b)))
	} else {
		g.emit(bytecode.MakeR(bytecode.RBin, uint8(op), r16(d), r16(a), r16(b)))
	}
	return d
}

func (g *gen) logical(n *ast.Logical, dst int) int {
	if n.Op == "??" {
		return g.nullish(n, dst)
	}
	d := g.dest(dst)
	end := g.newMark()
	g.into(n.Left, d)
	switch n.Op {
	case "&&":
		g.jump(bytecode.MakeR(bytecode.RJmpF, 0, r16(d), 0, 0), end, false)
	case "||":
		g.jump(bytecode.MakeR(bytecode.RJmpT, 0, r16(d), 0, 0), end, false)
	}
	g.into(n.Right, d)
	g.bind(end)
	return d
}

func (g *gen) unary(n *ast.Unary, dst int) int {
	switch n.Op {
	case "-":
		if v, ok := numberLit(n); ok {
			return g.number(v, dst)
		}
		return g.unaryOp(bytecode.OpNeg, n.Operand, dst, n.Start)
	case "+":
		return g.unaryOp(bytecode.OpPos, n.Operand, dst, n.Start)
	case "!":
		return g.unaryOp(bytecode.OpNot, n.Operand, dst, n.Start)
	case "~":
		return g.unaryOp(bytecode.OpBitNot, n.Operand, dst, n.Start)
	case "typeof":
		a := g.typeofOperand(n.Operand)
		d := g.dest(dst)
		g.at(n.Start)
		g.emit(bytecode.MakeR(bytecode.RTypeof, 0, r16(d), r16(a), 0))
		return d
	case "delete":
		switch o := n.Operand.(type) {
		case *ast.Member:
			return g.deleteMember(n, o, dst)
		case *ast.OptionalChain:
			if m, ok := o.Base.(*ast.Member); ok {
				return g.deleteChain(n, m, dst)
			}
		case *ast.Ident:
			return g.deleteName(n, o, dst)
		}
		// delete of anything but a property or a name is true, its
		// operand evaluated.
		g.discard(n.Operand)
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RLoadTrue, 0, r16(d), 0, 0))
		return d
	case "void":
		g.discard(n.Operand)
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(d), 0, 0))
		return d
	}
	g.fail("the unary operator " + n.Op)
	return g.dest(dst)
}

// importPhase reports whether n is import(...), import.defer(...) or
// import.source(...), and which: the parser writes each as a call of a
// name no script can write (the stack form's importPhase).
func importPhase(n *ast.Call) (int, bool) {
	id, ok := n.Callee.(*ast.Ident)
	if !ok {
		return 0, false
	}
	switch id.Name {
	case "import":
		return bytecode.ImportEvaluate, true
	case "import.defer":
		return bytecode.ImportDefer, true
	case "import.source":
		return bytecode.ImportSource, true
	}
	return 0, false
}

// importCall compiles an import call: its specifier and options in a
// window, the promise of the module made by import (cold), at the call's
// position, as the stack form's import.
func (g *gen) importCall(n *ast.Call, phase, dst int) int {
	argc := len(n.Args)
	if argc < 1 || argc > 2 || hasSpread(n.Args) {
		panic("regcomp: an import call of other than one or two arguments")
	}
	w := g.allocN(argc)
	for i, a := range n.Args {
		g.into(a, w+i)
	}
	d := g.dest(dst)
	g.at(n.TracePos())
	g.cold(bytecode.RImport, d, w, phase<<2|argc)
	return d
}

// deleteMember compiles delete o.k, delete o[k] and, inside an optional
// chain, delete o?.k.
func (g *gen) deleteMember(n *ast.Unary, m *ast.Member, dst int) int {
	if m.Optional {
		mm := *m
		mm.Optional = false
		mm.Object = g.optionalLink(m.Object)
		m = &mm
	}
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		g.deleteSuper(n, m)
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RLoadTrue, 0, r16(d), 0, 0))
		return d
	}
	o := g.expr(m.Object, -1)
	var k int
	if m.Computed {
		o = g.guard(o, m.Property)
		k = g.expr(m.Property, -1)
	} else {
		name := memberName(m)
		k = g.alloc()
		g.emit(bytecode.MakeRBC(bytecode.RLoadConst, 0, r16(k), g.constIdx(bytecode.Constant{Kind: bytecode.ConstString, Str: name})))
	}
	d := g.dest(dst)
	g.at(n.Start)
	g.emit(bytecode.MakeR(bytecode.RDelete, 0, r16(d), r16(o), r16(k)))
	return d
}

// deleteChain compiles delete of an optional chain's property, a?.b or
// a?.b.c: true where a link is nullish, as optionalChain's undefined.
func (g *gen) deleteChain(n *ast.Unary, m *ast.Member, dst int) int {
	saved := g.chain
	c := &chainCtx{nullish: g.newMark()}
	g.chain = c
	r := g.deleteMember(n, m, -1)
	g.chain = saved
	d := dst
	if d < 0 || g.isBinding(d) && d != r {
		d = g.alloc()
	}
	if r != d {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(r), 0))
	}
	end := g.newMark()
	g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), end, false)
	g.bind(c.nullish)
	g.emit(bytecode.MakeR(bytecode.RLoadTrue, 0, r16(d), 0, 0))
	g.bind(end)
	return g.place(d, dst)
}

// deleteName compiles delete of a name, which sloppy code alone has:
// false for a binding, and for a global what deleting the property gives
// (delete_var), as the stack form resolved it.
func (g *gen) deleteName(n *ast.Unary, id *ast.Ident, dst int) int {
	res, ok := g.rec.Read[id]
	if !ok {
		g.fail("a name the record does not resolve")
		return g.dest(dst)
	}
	d := g.dest(dst)
	switch res.Kind {
	case ResWith:
		g.probeOp(id, bytecode.OpWithDelete)
		g.probed(id, bytecode.RWithDelete, d, func() { g.deleteName(n, id, d) })
	case ResLocal, ResUpval:
		g.emit(bytecode.MakeR(bytecode.RLoadFalse, 0, r16(d), 0, 0))
	case ResGlobal:
		g.at(n.Start)
		g.far(g.emit(bytecode.MakeR(bytecode.RDeleteVar, 0, r16(d), g.name16(id.Name), 0)), id.Name)
	default:
		panic("regcomp: delete of a name resolved as " + fmt.Sprint(res.Kind))
	}
	return d
}

func (g *gen) unaryOp(op bytecode.Op, operand ast.Expr, dst, pos int) int {
	mark := g.top
	a := g.expr(operand, -1)
	d := g.result(mark, dst)
	g.at(pos)
	g.emit(bytecode.MakeR(bytecode.RUnary, uint8(op), r16(d), r16(a), 0))
	return d
}

// cond jumps to target where e's truthiness is when, and falls through
// otherwise.
func (g *gen) cond(e ast.Expr, when bool, target *mark) {
	mark := g.top
	defer func() { g.top = mark }()
	switch n := e.(type) {
	case *ast.Unary:
		if n.Op == "!" {
			g.cond(n.Operand, !when, target)
			return
		}
	case *ast.BoolLit:
		if n.Value == when {
			g.jump(bytecode.MakeR(bytecode.RJmp, 0, 0, 0, 0), target, false)
		}
		return
	case *ast.Logical:
		switch {
		case n.Op == "&&" && when:
			skip := g.newMark()
			g.cond(n.Left, false, skip)
			g.cond(n.Right, true, target)
			g.bind(skip)
			return
		case n.Op == "&&":
			g.cond(n.Left, false, target)
			g.cond(n.Right, false, target)
			return
		case n.Op == "||" && when:
			g.cond(n.Left, true, target)
			g.cond(n.Right, true, target)
			return
		case n.Op == "||":
			skip := g.newMark()
			g.cond(n.Left, true, skip)
			g.cond(n.Right, false, target)
			g.bind(skip)
			return
		}
	case *ast.Binary:
		if n.Op == "==" || n.Op == "!=" {
			// == null and != null: whether the value is null or undefined.
			other, isNull := n.Left, isNullish(n.Right)
			if !isNull {
				other, isNull = n.Right, isNullish(n.Left)
			}
			if isNull {
				a := g.expr(other, -1)
				op := bytecode.RJNull
				if (n.Op == "==") != when {
					op = bytecode.RJNotNull
				}
				g.at(n.Start)
				g.jump(bytecode.MakeR(op, 0, r16(a), 0, 0), target, true)
				return
			}
		}
		if x, i, ok := g.typeofCompare(n); ok {
			// typeof x === "name": the type asked of x, with no string
			// made or compared.
			a := g.typeofOperand(x)
			flag := uint8(i)
			if (n.Op == "===" || n.Op == "==") == when {
				flag |= bytecode.RCmpTrue
			}
			g.at(n.Start)
			g.jump(bytecode.MakeR(bytecode.RJTypeof, flag, r16(a), 0, 0), target, true)
			return
		}
		if op, ok := g.compareJump(n.Op, when); ok {
			a := g.guard(g.expr(n.Left, -1), n.Right)
			g.at(n.Start)
			if imm, ok := imm16(n.Right); ok {
				g.jump(bytecode.MakeR(op+immOffset(op), 0, r16(a), uint16(int16(imm)), 0), target, true)
				return
			}
			if u, ok := g.upvalOperand(n.Right); ok {
				if uop, ok := upvalJump[op]; ok {
					g.jump(bytecode.MakeR(uop, 0, r16(a), uint16(u), 0), target, true)
					return
				}
			}
			b := g.expr(n.Right, -1)
			g.at(n.Start)
			g.jump(bytecode.MakeR(op, 0, r16(a), r16(b), 0), target, true)
			return
		}
	}
	if c, ok := e.(*ast.Call); ok {
		g.dead = c
	}
	r := g.expr(e, -1)
	op := bytecode.RJmpF
	if when {
		op = bytecode.RJmpT
	}
	g.jump(bytecode.MakeR(op, 0, r16(r), 0, 0), target, false)
}

// upvalJump is the compare-and-branch of a register with an upvalue for
// each one of two registers that has one: a loop's bound a closure
// captures, read in the comparison rather than into a register first.
var upvalJump = map[bytecode.ROp]bytecode.ROp{
	bytecode.RJLt: bytecode.RJLtU, bytecode.RJNLt: bytecode.RJNLtU,
	bytecode.RJLe: bytecode.RJLeU, bytecode.RJNLe: bytecode.RJNLeU,
}

// upvalOperand is the upvalue e reads, where e is a name whose read is an
// upvalue's, which needs no check of its dead zone.
func (g *gen) upvalOperand(e ast.Expr) (uint32, bool) {
	id, ok := e.(*ast.Ident)
	if !ok {
		return 0, false
	}
	res, ok := g.rec.Read[id]
	if !ok || res.Kind != ResUpval || res.Index >= limit16 {
		return 0, false
	}
	return res.Index, true
}

// compareJump is the register form of the compare-and-branch that jumps
// when the comparison text is when.
func (g *gen) compareJump(text string, when bool) (bytecode.ROp, bool) {
	type pair struct{ yes, no bytecode.ROp }
	ops := map[string]pair{
		"<": {bytecode.RJLt, bytecode.RJNLt}, "<=": {bytecode.RJLe, bytecode.RJNLe},
		">": {bytecode.RJGt, bytecode.RJNGt}, ">=": {bytecode.RJGe, bytecode.RJNGe},
		"===": {bytecode.RJSEq, bytecode.RJSNe}, "!==": {bytecode.RJSNe, bytecode.RJSEq},
		"==": {bytecode.RJEq, bytecode.RJNe}, "!=": {bytecode.RJNe, bytecode.RJEq},
	}
	p, ok := ops[text]
	if !ok {
		return 0, false
	}
	if when {
		return p.yes, true
	}
	return p.no, true
}

// immOffset is how far a compare-and-branch's immediate form is from its
// register form.
func immOffset(op bytecode.ROp) bytecode.ROp {
	switch op {
	case bytecode.RJSEq, bytecode.RJSNe:
		return bytecode.RJSEqI - bytecode.RJSEq
	case bytecode.RJEq, bytecode.RJNe:
		return bytecode.RJEqI - bytecode.RJEq
	}
	return bytecode.RJLtI - bytecode.RJLt
}

// --- stores ---------------------------------------------------------------

// runsNoCode reports whether e's value is made without throwing or running
// a script's code: a literal, a function expression, or a read of a binding
// that needs no check of its initialisation.
func (g *gen) runsNoCode(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit, *ast.FuncLit:
		return true
	case *ast.Ident:
		res, ok := g.rec.Read[n]
		return ok && (res.Kind == ResLocal || res.Kind == ResUpval || res.Kind == ResLiteral && res.Lit == nil) &&
			!(g.fn.ForwardsArguments && g.isArguments(n))
	}
	_, ok := numberLit(e)
	return ok
}

// storeIdent stores value into the binding id names; init says it is the
// binding's declaration.
func (g *gen) storeIdent(id *ast.Ident, value ast.Expr, init bool) int {
	res, ok := g.rec.Write[id]
	if !ok {
		g.fail("a store the record does not resolve")
		return 0
	}
	switch res.Kind {
	case ResWith:
		return g.withAssign(id, value)
	case ResLocal, ResLocalInit:
		if k := g.farOf(res.Index); k >= 0 {
			v := g.expr(value, -1)
			g.movToFar(k, v)
			return v
		}
		r := int(g.slotReg[res.Index])
		g.into(value, r)
		return r
	case ResLocalCheck:
		// A binding in its dead zone throws as it is set, after the value
		// is made.
		v := g.expr(value, -1)
		g.storeValue(id, v)
		return v
	case ResUpval:
		v := g.expr(value, -1)
		g.emit(bytecode.MakeRBC(bytecode.RSetUpval, 0, r16(v), res.Index))
		return v
	case ResUpvalCheck:
		v := g.expr(value, -1)
		g.storeValue(id, v)
		return v
	case ResGlobal:
		var strict uint8
		if g.fn.Strict {
			// Whether the name resolves is settled before the value is
			// evaluated, which the stack form's checks do: a value that
			// runs no code cannot change the answer, and the store asks it
			// (set_global_strict's folding, foldGlobalCheck).
			if !g.runsNoCode(value) {
				// Settled before the value runs, reported after it, as
				// the stack form's check_global_ref and assert_resolved.
				ok := g.alloc()
				g.at(int(id.Start))
				name := g.name16(id.Name)
				g.far(g.emit(bytecode.MakeR(bytecode.RCheckGlobalRef, 0, r16(ok), name, g.globalSite("check_global_ref", id.Name))), id.Name)
				v := g.expr(value, -1)
				g.at(int(id.Start))
				g.far(g.emit(bytecode.MakeR(bytecode.RAssertResolved, 0, r16(ok), name, 0)), id.Name)
				g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, 0, r16(v), name, g.globalSite("RSetGlobal", id.Name))), id.Name)
				return v
			}
			strict = 1
		}
		v := g.expr(value, -1)
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, strict, r16(v), g.name16(id.Name), g.globalSite("RSetGlobal", id.Name))), id.Name)
		return v
	case ResGlobalVar:
		// A var's store: the var exists, and the store is set_global's
		// whatever the code's strictness, as the stack form's.
		v := g.expr(value, -1)
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, 0, r16(v), g.name16(id.Name), g.globalSite("RSetGlobal", id.Name))), id.Name)
		return v
	case ResGlobalLexInit, ResModuleLexInit:
		v := g.expr(value, -1)
		g.lexInit(res.Kind, id.Name, v, int(id.Start))
		return v
	case ResIgnore:
		return g.expr(value, -1)
	case ResConstAssign:
		v := g.expr(value, -1)
		g.assignConst(id)
		return v
	}
	g.fail("a store the backend does not compile")
	return 0
}

// freeze emits str_freeze of r, the read n, where the stack form follows
// that read by one: the binding is one an append grows, and what reads it
// here may keep what it read. A read the register form makes no value of
// -- a compare's operand, a step -- keeps nothing and is not frozen.
func (g *gen) freeze(n *ast.Ident, r int) {
	if g.rec.Freezes[n] {
		g.emit(bytecode.MakeR(bytecode.RStrFreeze, 0, r16(r), 0, 0))
	}
}

// appendLocal compiles n, an append to a local whose value nothing uses
// (Record's Appends), as append, and returns the local's register and
// whether it did.
func (g *gen) appendLocal(n *ast.Assign) (int, bool) {
	id, ok := n.Target.(*ast.Ident)
	if !ok {
		return 0, false
	}
	res, ok := g.rec.Write[id]
	if !ok || res.Kind != ResLocal || int(res.Index) >= len(g.slotReg) || g.slotReg[res.Index] < 0 || g.farOf(res.Index) >= 0 {
		return 0, false
	}
	r := int(g.slotReg[res.Index])
	v := g.expr(n.Value, -1)
	g.at(int(n.Start))
	g.emit(bytecode.MakeR(bytecode.RAppend, 0, r16(r), r16(v), 0))
	return r, true
}

func (g *gen) assign(n *ast.Assign, dst int, keep bool) int {
	if call, ok := n.Target.(*ast.Call); ok {
		// Sloppy code's assignment to a call, of any operator: the value
		// is never made.
		return g.place(g.callTarget(call, int(n.Start)), dst)
	}
	if !keep && dst < 0 && n.Op == "+=" && g.rec.Appends[n] {
		if r, ok := g.appendLocal(n); ok {
			return r
		}
	}
	if n.Op == "=" {
		switch t := n.Target.(type) {
		case *ast.Ident:
			r := g.storeIdent(t, n.Value, false)
			return g.place(r, dst)
		case *ast.ArrayPattern, *ast.ObjectPattern:
			v := g.patternSource(n.Value)
			g.pattern(t, v, false)
			return g.place(v, dst)
		case *ast.Member:
			if special(t) {
				return g.specialStore(t, n.Value, dst)
			}
			o := g.guard(g.expr(t.Object, -1), n.Value)
			if t.Computed {
				if k, ok := imm16(t.Property); ok && k >= 0 {
					v := g.expr(n.Value, -1)
					g.at(int(n.Start))
					g.emit(bytecode.MakeR(bytecode.RSetIndexI, 0, r16(o), uint16(k), r16(v)))
					return g.place(v, dst)
				}
				k := g.guard(g.expr(t.Property, -1), n.Value)
				// The value made in the place the assignment's own value
				// goes, where the store reads nothing there: lastX = x[i] =
				// e, with no move after the store.
				into := -1
				if dst >= 0 && dst != o && dst != k && direct(n.Value) {
					into = dst
				}
				v := g.expr(n.Value, into)
				g.at(int(n.Start))
				g.emit(bytecode.MakeR(bytecode.RSetIndex, 0, r16(o), r16(k), r16(v)))
				return g.place(v, dst)
			}
			name := memberName(t)
			v := g.expr(n.Value, -1)
			g.at(int(n.Start))
			g.emit(bytecode.MakeR(bytecode.RSetProp, 0, r16(o), r16(v), g.propSite(bytecode.OpSetProp, g.nameIdx(name))))
			return g.place(v, dst)
		}
		panic(fmt.Sprintf("regcomp: an assignment target %T", n.Target))
	}
	switch n.Op {
	case "||=", "&&=", "??=":
		return g.logicalAssign(n, dst)
	}
	text := n.Op[:len(n.Op)-1]
	op, ok := binOps[text]
	if !ok {
		panic("regcomp: an assignment operator " + n.Op)
	}
	switch t := n.Target.(type) {
	case *ast.Ident:
		res := g.rec.Write[t]
		if res.Kind == ResLocal && g.farOf(res.Index) < 0 {
			r := int(g.slotReg[res.Index])
			if rr := g.rec.Read[t]; rr.Kind != ResLocal || rr.Index != res.Index {
				g.fail("a compound assignment's read")
				return 0
			}
			a := g.guard(r, n.Value)
			g.operate(text, op, nil, n.Value, a, r, int(n.Start))
			return g.place(r, dst)
		}
		old := g.guard(g.readForWrite(t, -1), n.Value)
		v := g.operate(text, op, nil, n.Value, old, -1, int(n.Start))
		g.storeValue(t, v)
		return g.place(v, dst)
	case *ast.Member:
		if special(t) {
			return g.specialCompound(t, text, op, n.Value, int(n.Start), dst)
		}
		o := g.guard(g.expr(t.Object, -1), n.Value)
		var k int
		var name uint32
		if t.Computed {
			k = g.key(t, g.guard(g.expr(t.Property, -1), n.Value), o)
		} else {
			s := memberName(t)
			name = g.nameIdx(s)
		}
		cur := g.alloc()
		g.at(t.Property.Pos())
		if t.Computed {
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(cur), r16(o), r16(k)))
		} else {
			g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(cur), r16(o), g.propSite(bytecode.OpGetProp, name)))
		}
		v := g.operate(text, op, nil, n.Value, cur, cur, int(n.Start))
		g.at(int(n.Start))
		if t.Computed {
			g.emit(bytecode.MakeR(bytecode.RSetIndex, 0, r16(o), r16(k), r16(v)))
		} else {
			g.emit(bytecode.MakeR(bytecode.RSetProp, 0, r16(o), r16(v), g.propSite(bytecode.OpSetProp, name)))
		}
		return g.place(v, dst)
	}
	panic(fmt.Sprintf("regcomp: a compound assignment target %T", n.Target))
}

// callTarget compiles call, which sloppy code assigned to, updated or
// named as a for-in or for-of head's target, as the stack form's
// compileCallTarget: the call is made, and then the assignment is a
// ReferenceError, at position pos. The call's value, in the register it
// returns, stands for the expression's on the path nothing reaches.
func (g *gen) callTarget(call *ast.Call, pos int) int {
	r := g.expr(call, -1)
	g.at(pos)
	g.far(g.cold(bytecode.RThrowRef, 0, int(g.name16("invalid assignment target")), 0), "invalid assignment target")
	return r
}

// addProp compiles o.name += value, or, with value nil, o.name += imm, whose
// value is unused, as one add_prop: where value is a constant or a binding
// no one else writes, which reading the property first would not see
// change, so that reading it in the operation is the same.
func (g *gen) addProp(m *ast.Member, value ast.Expr, imm int32, pos int) bool {
	name, ok := propName(m)
	if !ok {
		return false
	}
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		return false
	}
	var v int
	op := bytecode.RAddPropI
	if value != nil {
		if i, ok := imm16(value); ok {
			imm = i
		} else {
			id, isIdent := value.(*ast.Ident)
			if !isIdent {
				return false
			}
			res, ok := g.rec.Read[id]
			if !ok || res.Kind != ResLocal || g.fn.Locals[res.Index].Captured {
				return false
			}
			op = bytecode.RAddProp
		}
	}
	o := g.expr(m.Object, -1)
	if op == bytecode.RAddProp {
		v = g.expr(value, -1)
	}
	g.at(pos)
	site := g.site16(siteKey{kind: "add_prop", name: g.nameIdx(name)})
	if op == bytecode.RAddProp {
		g.emit(bytecode.MakeR(op, 0, r16(o), r16(v), site))
	} else {
		// X says an update, ++ or --, which converts by ToNumeric where +=
		// adds as + does.
		x := uint8(0)
		if value == nil {
			x = 1
		}
		g.emit(bytecode.MakeR(op, x, r16(o), uint16(int16(imm)), site))
	}
	return true
}

// addIndex compiles o[k] += value and o[k]++ as addProp does o.name's, one
// add_index: the key in a register, or an index of 16 bits (add_index_k),
// and the value a binding no one else writes or a constant, an immediate
// where the key is in a register (add_index_imm).
func (g *gen) addIndex(m *ast.Member, value ast.Expr, imm int32, pos int) bool {
	if !m.Computed {
		return false
	}
	if _, isSuper := m.Object.(*ast.Super); isSuper {
		return false
	}
	x := uint8(0)
	valImm := value == nil
	if valImm {
		x = 1
	} else if i, ok := imm16(value); ok {
		imm, valImm = i, true
	} else {
		id, isIdent := value.(*ast.Ident)
		if !isIdent {
			return false
		}
		res, ok := g.rec.Read[id]
		if !ok || res.Kind != ResLocal || g.fn.Locals[res.Index].Captured {
			return false
		}
	}
	key, keyImm := imm16(m.Property)
	keyImm = keyImm && key >= 0
	o := g.expr(m.Object, -1)
	if !keyImm {
		o = g.guard(o, m.Property)
		k := g.expr(m.Property, -1)
		if valImm {
			g.readStore(m, pos, bytecode.MakeR(bytecode.RAddIndexI, x, r16(o), r16(k), uint16(int16(imm))))
			return true
		}
		v := g.expr(value, -1)
		g.readStore(m, pos, bytecode.MakeR(bytecode.RAddIndex, 0, r16(o), r16(k), r16(v)))
		return true
	}
	var v int
	if valImm {
		v = g.number(float64(imm), -1)
	} else {
		v = g.expr(value, -1)
	}
	g.readStore(m, pos, bytecode.MakeR(bytecode.RAddIndexK, x, r16(o), r16(v), uint16(key)))
	return true
}

// readStore emits w, which reads m and stores to it: placed at m's key,
// where the stack form reads it, with the store's position, the
// statement's, kept for its stack instruction (Aux), where an error of the
// store is placed.
func (g *gen) readStore(m *ast.Member, pos int, w bytecode.RInstr) {
	g.at(m.Property.Pos())
	at := g.emit(w)
	if g.stores == nil {
		g.stores = map[int]int32{}
	}
	g.stores[at] = int32(pos)
}

// readForWrite reads the binding a compound assignment or an update of
// id names, which the stack form may have read with the store's own
// resolution rather than a read of its own.
func (g *gen) readForWrite(id *ast.Ident, dst int) int {
	if w := g.withs[id]; w != nil && g.rec.Write[id].Kind == ResWith {
		g.withRefFor(id)
		return g.refRead(id, g.withRefs[id], dst)
	}
	if _, ok := g.rec.Read[id]; ok {
		return g.ident(id, dst)
	}
	res := g.rec.Write[id]
	d := g.dest(dst)
	switch res.Kind {
	case ResUpval:
		g.emit(bytecode.MakeRBC(bytecode.RGetUpval, 0, r16(d), res.Index))
	case ResUpvalCheck:
		g.at(int(id.Start))
		g.emit(bytecode.MakeRBC(bytecode.RGetUpvalCheck, 0, r16(d), res.Index))
	case ResGlobal:
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RGetGlobal, 0, r16(d), g.name16(id.Name), g.globalSite("RGetGlobal", id.Name))), id.Name)
	default:
		g.fail("a compound assignment's read")
	}
	return d
}

// storeValue stores register v into the binding id names.
func (g *gen) storeValue(id *ast.Ident, v int) {
	res := g.rec.Write[id]
	switch res.Kind {
	case ResWith:
		g.withStore(id, v)
	case ResLocal, ResLocalInit:
		if k := g.farOf(res.Index); k >= 0 {
			g.movToFar(k, v)
			return
		}
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(int(g.slotReg[res.Index])), r16(v), 0))
	case ResLocalCheck:
		r := int(g.slotReg[res.Index])
		g.at(int(id.Start))
		g.checkRef(r, res.Index)
		if !g.fn.Locals[res.Index].Mutable {
			// A constant past its dead zone, as set_local_check finds it:
			// a switch case's, which the dead zone marks.
			g.far(g.emit(bytecode.MakeR(bytecode.RAssignConst, 0, 0, g.name16(id.Name), 0)), id.Name)
			return
		}
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(r), r16(v), 0))
	case ResUpval:
		g.emit(bytecode.MakeRBC(bytecode.RSetUpval, 0, r16(v), res.Index))
	case ResUpvalCheck:
		g.at(int(id.Start))
		g.emit(bytecode.MakeRBC(bytecode.RSetUpvalCheck, 0, r16(v), res.Index))
	case ResGlobalVar:
		// A var's store: set_global whatever the code's strictness.
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, 0, r16(v), g.name16(id.Name), g.globalSite("RSetGlobal", id.Name))), id.Name)
	case ResGlobalLexInit, ResModuleLexInit:
		g.lexInit(res.Kind, id.Name, v, int(id.Start))
	case ResGlobal:
		// An update's or a compound assignment's store, after its read has
		// resolved the name: in strict code the store asks again, and a
		// binding the value's code deleted is a ReferenceError.
		var strict uint8
		if g.fn.Strict {
			strict = 1
		}
		g.at(int(id.Start))
		g.far(g.emit(bytecode.MakeR(bytecode.RSetGlobal, strict, r16(v), g.name16(id.Name), g.globalSite("RSetGlobal", id.Name))), id.Name)
	case ResConstAssign:
		g.assignConst(id)
	case ResIgnore:
	default:
		g.fail("a store the backend does not compile")
	}
}

// assignConst throws as an assignment to the constant id does.
func (g *gen) assignConst(id *ast.Ident) {
	g.at(int(id.Start))
	g.far(g.emit(bytecode.MakeR(bytecode.RAssignConst, 0, 0, g.name16(id.Name), 0)), id.Name)
}

func (g *gen) update(n *ast.Update, keep bool, dst int) int {
	step := bytecode.RInc
	if n.Op == "--" {
		step = bytecode.RDec
	}
	switch t := n.Operand.(type) {
	case *ast.Call:
		return g.place(g.callTarget(t, n.Start), dst)
	case *ast.Ident:
		res := g.rec.Write[t]
		if res.Kind == ResLocal && g.farOf(res.Index) < 0 {
			r := int(g.slotReg[res.Index])
			g.at(n.Start)
			if !keep || n.Prefix {
				g.emit(bytecode.MakeR(step, 0, r16(r), r16(r), 0))
				return g.place(r, dst)
			}
			old := g.dest(dst)
			if old == r {
				old = g.alloc()
			}
			post := bytecode.RPostInc
			if step == bytecode.RDec {
				post = bytecode.RPostDec
			}
			g.emit(bytecode.MakeR(post, 0, r16(old), r16(r), 0))
			return g.place(old, dst)
		}
		cur := g.readForWrite(t, g.alloc())
		return g.stepped(n, cur, step, keep, dst, func(v int) { g.storeValue(t, v) })
	case *ast.Member:
		if special(t) {
			return g.specialUpdate(n, t, step, keep, dst)
		}
		o := g.expr(t.Object, -1)
		cur := g.alloc()
		if t.Computed {
			k := g.key(t, g.expr(t.Property, -1), o)
			g.at(int(t.Start))
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(cur), r16(o), r16(k)))
			return g.stepped(n, cur, step, keep, dst, func(v int) {
				g.at(n.Start)
				g.emit(bytecode.MakeR(bytecode.RSetIndex, 0, r16(o), r16(k), r16(v)))
			})
		}
		s := memberName(t)
		name := g.nameIdx(s)
		g.at(int(t.Start))
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(cur), r16(o), g.propSite(bytecode.OpGetProp, name)))
		return g.stepped(n, cur, step, keep, dst, func(v int) {
			g.at(n.Start)
			g.emit(bytecode.MakeR(bytecode.RSetProp, 0, r16(o), r16(v), g.propSite(bytecode.OpSetProp, name)))
		})
	}
	panic(fmt.Sprintf("regcomp: an update target %T", n.Operand))
}

// stepped is an update of the value in cur, stored by store.
func (g *gen) stepped(n *ast.Update, cur int, step bytecode.ROp, keep bool, dst int, store func(int)) int {
	g.at(n.Start)
	if keep && !n.Prefix {
		old := g.alloc()
		g.emit(bytecode.MakeR(bytecode.RUnary, uint8(bytecode.OpToNumeric), r16(old), r16(cur), 0))
		g.emit(bytecode.MakeR(step, 0, r16(cur), r16(old), 0))
		store(cur)
		return g.place(old, dst)
	}
	g.emit(bytecode.MakeR(step, 0, r16(cur), r16(cur), 0))
	store(cur)
	return g.place(cur, dst)
}

// --- properties, calls, literals ------------------------------------------

// memberName is the name of m, a member that is neither computed nor
// private: the parser gives such a member an identifier as its property,
// so anything else is the parser's bug.
func memberName(m *ast.Member) string {
	name, ok := propName(m)
	if !ok {
		panic("regcomp: a member's property that is not a name")
	}
	return name
}

func propName(m *ast.Member) (string, bool) {
	if m.Computed {
		return "", false
	}
	id, ok := m.Property.(*ast.Ident)
	if !ok {
		return "", false
	}
	return id.Name, true
}

// preStepped is the register and step of e where e is ++i or --i of a
// local i, which an element's read steps in place (RGetIndexPre).
func (g *gen) preStepped(e ast.Expr) (int, int8, bool) {
	return g.localStep(e, true)
}

// localStep is the register and step of e where e is an update of a local
// i, prefix or postfix as prefix says.
func (g *gen) localStep(e ast.Expr, prefix bool) (int, int8, bool) {
	u, ok := e.(*ast.Update)
	if !ok || u.Prefix != prefix {
		return 0, 0, false
	}
	id, ok := u.Operand.(*ast.Ident)
	if !ok {
		return 0, 0, false
	}
	res := g.rec.Write[id]
	if res.Kind != ResLocal || g.farOf(res.Index) >= 0 {
		return 0, 0, false
	}
	if u.Op == "--" {
		return int(g.slotReg[res.Index]), -1, true
	}
	return int(g.slotReg[res.Index]), 1, true
}

func (g *gen) member(n *ast.Member, dst int) int {
	if n.Optional {
		m := *n
		m.Optional = false
		m.Object = g.optionalLink(n.Object)
		return g.member(&m, dst)
	}
	if id, ok := n.Object.(*ast.Ident); ok {
		if res := g.rec.Read[id]; res.Kind == ResLazyArgs {
			// The arguments read where they lie, the object's local
			// undefined until something makes it (arguments_index).
			a := int(g.slotReg[res.Index])
			if !n.Computed {
				d := g.dest(dst)
				g.at(n.Property.Pos())
				g.emit(bytecode.MakeR(bytecode.RArgsLen, 0, r16(d), r16(a), 0))
				return d
			}
			k := g.expr(n.Property, -1)
			d := g.dest(dst)
			g.at(n.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RArgsIndex, 0, r16(d), r16(a), r16(k)))
			return d
		}
	}
	if special(n) {
		return g.specialMember(n, dst)
	}
	o := g.expr(n.Object, -1)
	if n.Computed {
		if k, ok := imm16(n.Property); ok && k >= 0 {
			d := g.dest(dst)
			g.at(n.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndexI, 0, r16(d), r16(o), uint16(k)))
			return d
		}
		o = g.guard(o, n.Property)
		if k, step, ok := g.preStepped(n.Property); ok {
			// x[++i] of a local i: the step and the read as one operation.
			d := g.dest(dst)
			g.at(n.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndexPre, uint8(step), r16(d), r16(o), r16(k)))
			return d
		}
		if k, step, ok := g.localStep(n.Property, false); ok {
			// x[i++] of a local i: the read and the step as one operation.
			d := g.dest(dst)
			g.at(n.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndexPost, uint8(step), r16(d), r16(o), r16(k)))
			return d
		}
		k := g.expr(n.Property, -1)
		d := g.dest(dst)
		g.at(n.Property.Pos())
		g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(d), r16(o), r16(k)))
		return d
	}
	name := memberName(n)
	d := g.dest(dst)
	g.at(n.Property.Pos())
	if name == "length" {
		g.emit(bytecode.MakeR(bytecode.RGetLength, 0, r16(d), r16(o), 0))
		return d
	}
	g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(d), r16(o), g.propSite(bytecode.OpGetProp, g.nameIdx(name))))
	return d
}

// directEval compiles a call the stack form made a direct eval: the
// callee, undefined as its receiver and the arguments -- or an array of
// them, where one is a spread -- in a window, and direct_eval (cold) at
// the call's position, its scope's index in Aux.
func (g *gen) directEval(n *ast.Call, idx uint32, dst int) int {
	argc := len(n.Args)
	spread := wideArgs(n.Args)
	if spread {
		argc = 1
		idx |= bytecode.RDirectEvalSpread
	}
	w := g.allocN(2 + argc)
	static := func() {
		g.into(n.Callee, w)
		g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w+1), 0, 0))
	}
	if id, ok := n.Callee.(*ast.Ident); ok && g.withs[id] != nil {
		// A with object's own eval is called with it as the receiver.
		g.probeOp(id, bytecode.OpWithGetThis)
		g.probed(id, bytecode.RWithGetThis, w, static)
	} else {
		static()
	}
	if spread {
		g.spreadArray(n.Args, w+2)
		argc = 0
	} else {
		g.args(n.Args, w+2)
	}
	d := g.dest(dst)
	g.at(int(n.Start))
	at := g.cold(bytecode.RDirectEval, w, d, argc)
	g.aux(at, idx)
	if g.rec.Tails[n] {
		g.mark(at, bytecode.RMarkTail)
	}
	return d
}

func (g *gen) args(args []ast.Expr, at int) {
	// Each argument in its place, what computing it took given back
	// before the next (into's registers, below the mark, stay).
	mark := g.top
	for i, a := range args {
		if _, ok := a.(*ast.Spread); ok {
			g.fail("a spread argument")
			return
		}
		g.into(a, at+i)
		g.top = mark
	}
}

func (g *gen) call(n *ast.Call, dst int) int {
	dead := g.dead == n
	g.dead = nil
	if phase, ok := importPhase(n); ok {
		return g.importCall(n, phase, dst)
	}
	if idx, ok := g.rec.Evals[n]; ok {
		return g.directEval(n, idx, dst)
	}
	if n.Optional || isChainCall(n) {
		return g.chainCall(n, dst)
	}
	if isSuper(n.Callee) {
		return g.superCall(n, dst)
	}
	if wideArgs(n.Args) {
		return g.spreadCall(n, dst)
	}
	if m, ok := n.Callee.(*ast.Member); ok && g.fn.ForwardsArguments && len(n.Args) == 2 && g.isArguments(n.Args[1]) {
		if name, ok := propName(m); ok && name == "apply" && !m.Computed && !m.Optional {
			return g.applyArgs(n, m, dst)
		}
	}
	if d, ok := g.charCodeAt(n, dst); ok {
		return d
	}
	if d, ok := g.regexpLitCall(n, dst, dead); ok {
		return d
	}
	argc := len(n.Args)
	// A receiver computed -- a call's, as each of a chain of calls'
	// a.f(x).g(y)... is the one before it -- is computed before the window
	// is taken, so that what computing it takes is given back first
	// (freeAbove) and the windows of a chain are not one above another:
	// get_method_w puts it in the window.
	recv, recvTemp := -1, false
	if c, ok := n.Callee.(*ast.Member); ok && !c.Optional && !special(c) && !c.Computed && !g.inPlace(c.Object) {
		m := g.top
		recv = g.expr(c.Object, -1)
		// A temporary of its own, or a binding's register that an
		// assignment left it in -- (o = x).f() -- which the call's value
		// must not take.
		recvTemp = recv >= m
	}
	w := g.allocN(2 + argc)
	op := bytecode.RCallW
	argLit, mname := false, ""
	switch c := n.Callee.(type) {
	case *ast.Member:
		if c.Optional {
			g.fail("an optional chain")
			return w
		}
		if special(c) {
			g.specialCallee(c, w)
		} else if recv >= 0 {
			g.at(c.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetMethodW, 0, r16(w), r16(recv), g.propSite(bytecode.OpGetPropThis, g.nameIdx(memberName(c)))))
		} else if !c.Computed && g.inPlace(c.Object) {
			// A receiver in a binding: get_method_w puts it in the window.
			name := memberName(c)
			o := g.expr(c.Object, -1)
			g.at(c.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetMethodW, 0, r16(w), r16(o), g.propSite(bytecode.OpGetPropThis, g.nameIdx(name))))
		} else if g.into(c.Object, w+1); c.Computed {
			k := g.expr(c.Property, -1)
			g.at(c.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetIndex, 0, r16(w), r16(w+1), r16(k)))
		} else {
			name := memberName(c)
			g.at(c.Property.Pos())
			g.emit(bytecode.MakeR(bytecode.RGetMethod, 0, r16(w), r16(w+1), g.propSite(bytecode.OpGetPropThis, g.nameIdx(name))))
		}
		op = bytecode.RCallMW
		if name, ok := propName(c); ok && !c.Computed {
			mname = name
			if argc > 0 && (name == "replace" || name == "split" || name == "match") {
				_, argLit = n.Args[0].(*ast.RegexpLit)
			}
		}
	case *ast.Ident:
		if g.withs[c] != nil && g.withs[c].probe.Op == bytecode.OpWithGetThis {
			// Through the object that has the name, which is the call's
			// receiver, as the stack form's with_get_this.
			g.probed(c, bytecode.RWithGetThis, w, func() {
				g.into(c, w)
				g.emit(bytecode.MakeR(bytecode.RLoadUndef, 0, r16(w+1), 0, 0))
			})
			op = bytecode.RCallMW
			break
		}
		g.into(c, w)
	default:
		g.into(n.Callee, w)
	}
	if argLit {
		// The literal is made by call_arg_lit: nothing can see it made
		// after the arguments that follow it.
		g.args(n.Args[1:], w+3)
	} else {
		g.args(n.Args, w+2)
	}
	if op == bytecode.RCallMW && argc == 2 && (mname == "asUintN" || mname == "asIntN") && len(g.code) > 0 {
		// X.asUintN(w, x op y): the operator, the arguments' last
		// instruction, may make the call's value at once (RWrap).
		if l := g.code[len(g.code)-1]; int(l.A()) == w+3 && wrapsOp(l.Op()) {
			g.code[len(g.code)-1] = bytecode.MakeR(bytecode.RWrap, 0, l.A(), l.B(), l.C())
			g.aux(len(g.code)-1, uint32(l.Op()))
		}
	}
	d := dst
	if d < 0 {
		d = w
		if recvTemp {
			// The receiver's register, below the window, which the window
			// has its copy of: a chain's calls then each leave their value
			// where the one before left its.
			d = recv
		}
	}
	g.at(n.TracePos())
	dead = dead && (mname == "exec" || mname == "replace")
	if g.rec.Tails[n] && !argLit {
		// A call in tail position, whose frame is given up once the
		// stack is deep, as the stack form's tail_call gives it up.
		op = tailOp(op)
	}
	at := g.emit(bytecode.MakeR(op, uint8(argc), r16(w), r16(d), g.callSite(op)))
	if argLit {
		// A RegExp literal given to replace, split or match, made by the
		// call, which may have its RegExp back once it is done with it.
		g.aux(at, g.regexpConst(n.Args[0].(*ast.RegexpLit)))
		g.mark(at, bytecode.RMarkLitArg|deadMark(dead))
	} else if dead {
		// A call of exec or replace whose value nothing reads but its
		// truth: a mark the call of the built-in reads.
		g.mark(at, bytecode.RMarkDead)
	} else if mname == "exec" && dst >= 0 {
		if spc, ok := g.reuseCall(n, dst); ok {
			g.aux(at, spc)
			g.mark(at, bytecode.RMarkReuse)
			g.reuses = true
		}
	}
	return d
}

// deadMark is the mark of a regexp call that says nothing reads its value
// but its truth.
func deadMark(dead bool) uint8 {
	if dead {
		return bytecode.RMarkDead
	}
	return 0
}

// mark gives the call at pc marks m (RegCode.Marks).
func (g *gen) mark(pc int, m uint8) {
	if g.marks == nil {
		g.marks = map[int]uint8{}
	}
	g.marks[pc] |= m
}

// reuseCall finds the stack form's call of exec n compiles to, whose value
// the next instruction stores in the local whose register is dst, and
// returns its pc, which the runtime asks whether its arrays may be made
// again (execReuseOf). It is the one call_method at n's position.
func (g *gen) reuseCall(n *ast.Call, dst int) (uint32, bool) {
	local := -1
	for i, r := range g.slotReg {
		if int(r) == dst && !g.fn.Locals[i].Captured {
			local = i
		}
	}
	if local < 0 {
		return 0, false
	}
	code, pos, found := g.fn.Code, int32(n.TracePos()), -1
	for i, l := range g.fn.Lines {
		if l.Pos != pos {
			continue
		}
		end := uint32(len(code))
		if i+1 < len(g.fn.Lines) {
			end = g.fn.Lines[i+1].PC
		}
		for pc := l.PC; pc < end && int(pc) < len(code); pc++ {
			if code[pc].Op == bytecode.OpCallMethod {
				if found >= 0 {
					return 0, false
				}
				found = int(pc)
			}
		}
	}
	if found < 0 || found+1 >= len(code) || code[found+1].A != uint32(local) {
		return 0, false
	}
	switch code[found+1].Op {
	case bytecode.OpSetLocal, bytecode.OpPutLocal, bytecode.OpSetLocalGet, bytecode.OpInitLocal:
		return uint32(found), true
	}
	return 0, false
}

// regexpConst is the constant of the RegExp literal n.
func (g *gen) regexpConst(n *ast.RegexpLit) uint32 {
	return g.constIdx(bytecode.Constant{Kind: bytecode.ConstRegExp, Str: n.Pattern, Flags: n.Flags})
}

// regexpLitCall compiles /x/.exec(...) and /x/.test(...) as lit_exec or
// lit_test and call_lit, the literal's RegExp the receiver, and reports
// false for any other call: the method is not read where it is the
// built-in, and the RegExp, where the built-in given a primitive was
// called, is the literal's again.
func (g *gen) regexpLitCall(n *ast.Call, dst int, dead bool) (int, bool) {
	m, ok := n.Callee.(*ast.Member)
	if !ok || m.Computed || m.Optional {
		return 0, false
	}
	lit, ok := m.Object.(*ast.RegexpLit)
	if !ok {
		return 0, false
	}
	name, ok := propName(m)
	if !ok || name != "exec" && name != "test" {
		return 0, false
	}
	k := g.regexpConst(lit)
	if k >= limit16 {
		return 0, false
	}
	if len(n.Args) == 1 && g.unchanging(n.Args[0]) {
		// One argument that the method's read cannot change, put in the
		// window first: the literal made, its method read and called by
		// one operation.
		w := g.allocN(3)
		g.into(n.Args[0], w+2)
		d := dst
		if d < 0 {
			d = w
		}
		x := uint8(bytecode.RLitCall) | litTest(name)
		if dead && name == "exec" {
			x |= bytecode.RLitDead
		}
		g.at(n.TracePos())
		at := g.emit(bytecode.MakeR(bytecode.RLit, x, r16(w), r16(d), uint16(k)))
		if g.stores == nil {
			g.stores = map[int]int32{}
		}
		g.stores[at] = int32(lit.Start)
		return d, true
	}
	argc := len(n.Args)
	w := g.allocN(2 + argc)
	g.at(lit.Start)
	g.emit(bytecode.MakeR(bytecode.RLit, litTest(name), r16(w), 0, uint16(k)))
	g.args(n.Args, w+2)
	d := dst
	if d < 0 {
		d = w
	}
	g.at(n.TracePos())
	at := g.emit(bytecode.MakeR(bytecode.RCallMW, uint8(argc), r16(w), r16(d), g.callSite(bytecode.RCallMW)))
	g.aux(at, k)
	g.mark(at, bytecode.RMarkLitRecv|deadMark(dead && name == "exec"))
	return d, true
}

// wrapsOp reports whether op is an operator RWrap may make: one the
// tree's wrapCall makes BigInt.asUintN's and asIntN's of from its
// operands (wrapsOperator).
func wrapsOp(op bytecode.ROp) bool {
	switch op {
	case bytecode.RAdd, bytecode.RSub, bytecode.RMul,
		bytecode.RBitAnd, bytecode.RBitOr, bytecode.RBitXor, bytecode.RShl, bytecode.RShr:
		return true
	}
	return false
}

// tailOp is call op's form in tail position.
func tailOp(op bytecode.ROp) bytecode.ROp {
	if op == bytecode.RCallMW {
		return bytecode.RCallMWT
	}
	return bytecode.RCallWT
}

// litTest is regexp_lit's flag of a literal's test, for name test.
func litTest(name string) uint8 {
	if name == "test" {
		return bytecode.RLitTest
	}
	return 0
}

// unchanging reports whether e's value, read before a method's read, is
// what it would be read after it: a constant, or a local of the function's
// own that nothing captures, in a function with no arguments object to
// write it through, and that needs no check of its initialization -- which
// the read, a getter, could neither write nor see uninitialized.
func (g *gen) unchanging(e ast.Expr) bool {
	switch n := e.(type) {
	case *ast.NumberLit, *ast.StringLit, *ast.BoolLit, *ast.NullLit:
		return true
	case *ast.Ident:
		res, ok := g.rec.Read[n]
		return ok && res.Kind == ResLocal && !g.fn.Locals[res.Index].Captured && !g.fn.UsesArguments
	}
	return false
}

// charCodeAt compiles recv.charCodeAt(i), a receiver in a binding and its
// argument a local no one else writes in one of the first 256 registers, as
// char_code_at, and reports false for any other call. The argument is read
// after the method, as the call reads it, and nothing the read runs can
// write it.
func (g *gen) charCodeAt(n *ast.Call, dst int) (int, bool) {
	m, ok := n.Callee.(*ast.Member)
	if !ok || m.Computed || m.Optional || len(n.Args) != 1 || !g.inPlace(m.Object) {
		return 0, false
	}
	if _, ok := m.Object.(*ast.Super); ok {
		return 0, false
	}
	if name, ok := propName(m); !ok || name != "charCodeAt" {
		return 0, false
	}
	id, ok := n.Args[0].(*ast.Ident)
	if !ok {
		return 0, false
	}
	res, ok := g.rec.Read[id]
	if !ok || res.Kind != ResLocal || g.fn.Locals[res.Index].Captured || g.slotReg[res.Index] >= 256 {
		return 0, false
	}
	o := g.expr(m.Object, -1)
	a := g.expr(id, -1)
	if a >= 256 {
		g.fail("char_code_at's argument past register 255")
		return g.dest(dst), true
	}
	d := g.dest(dst)
	site := g.propSite(bytecode.OpGetPropThis, g.nameIdx("charCodeAt"))
	g.at(n.TracePos())
	at := g.emit(bytecode.MakeR(bytecode.RCharCodeAt, uint8(a), r16(d), r16(o), site))
	if g.stores == nil {
		g.stores = map[int]int32{}
	}
	g.stores[at] = int32(m.Property.Pos())
	return d, true
}

// key is the register of the key k of t, a computed member read and then
// written, converted once to a property key with its object o checked
// first (key_of_base), as the stack form's to_property_key_of_base: the
// read and the write would otherwise each convert an object's key, and
// run its toString twice. A literal key needs nothing; a binding's value
// is converted into a temporary, the binding left as it is. A key that an
// operator made is a primitive, whose conversion runs nothing and which a
// null or undefined object fails on in the read as in key_of_base, so it
// needs nothing either: this.t-1 is converted once whichever way.
func (g *gen) key(t *ast.Member, k, o int) int {
	switch t.Property.(type) {
	case *ast.StringLit, *ast.NumberLit, *ast.Binary, *ast.Unary, *ast.Update, *ast.TemplateLit:
		return k
	}
	d := k
	if k < g.topAfterBindings() {
		d = g.alloc()
	}
	g.at(t.Property.Pos())
	g.emit(bytecode.MakeR(bytecode.RKeyOfBase, 0, r16(d), r16(o), r16(k)))
	return d
}

// maxChainParts is the most parts stringChain joins in one concat, as the
// stack form's compileStringChain: a longer chain is joined a run at a
// time, each run's string the first part of the next.
const maxChainParts = 32

// stringChain compiles a + b + c + ..., once a string literal among its
// operands has made every addition from there on a concatenation, as one
// concat of the parts in consecutive registers, as the stack form's
// compileStringChain does and with its conversions where it makes them:
// the operands before the literal added as they were and their sum
// converted where the literal was added to it, each operand after it
// converted as soon as it is evaluated (str_part), but a literal or a
// number, which concat writes out itself. It reports false, having emitted
// nothing, where the stack form makes the additions.
func (g *gen) stringChain(n *ast.Binary, dst int) (int, bool) {
	var adds []*ast.Binary
	for b := n; ; {
		adds = append(adds, b)
		l, ok := b.Left.(*ast.Binary)
		if !ok || l.Op != "+" {
			break
		}
		b = l
	}
	if len(adds) < 2 {
		return 0, false
	}
	slices.Reverse(adds)
	operand := func(i int) ast.Expr {
		if i == 0 {
			return adds[0].Left
		}
		return adds[i-1].Right
	}
	first := -1
	for i := range len(adds) + 1 {
		if _, ok := operand(i).(*ast.StringLit); ok {
			first = i
			break
		}
		if _, ok := operand(i).(*ast.TemplateLit); ok && i == 0 {
			// The stack form's chain starts at a template, which this
			// backend leaves to it.
			return 0, false
		}
	}
	if first < 0 {
		return 0, false
	}
	empty := func(e ast.Expr) bool {
		s, ok := e.(*ast.StringLit)
		return ok && s.Value == ""
	}
	parts := 0
	if first > 0 {
		parts++
	}
	for i := first; i <= len(adds); i++ {
		if !empty(operand(i)) {
			parts++
		}
	}
	if parts < 3 {
		return 0, false
	}
	w := g.top
	k := 0
	// next is the register of the next part, taken before the part is
	// compiled into it, so that what the part computes goes past it.
	next := func() int {
		g.top = w + k
		return g.alloc()
	}
	part := func() {
		k++
		if k == maxChainParts {
			g.at(int(n.Start))
			g.emit(bytecode.MakeR(bytecode.RConcat, 0, r16(w), r16(w), uint16(k)))
			k = 1
		}
	}
	convert := func(pos int) {
		g.at(pos)
		g.emit(bytecode.MakeR(bytecode.RStrPart, 0, r16(w+k), 0, 0))
	}
	if first > 0 {
		if first == 1 {
			g.into(operand(0), next())
		} else {
			g.into(adds[first-2], next())
		}
		convert(adds[first-1].Start)
		part()
	}
	for i := first; i <= len(adds); i++ {
		switch e := operand(i); e.(type) {
		case *ast.StringLit:
			if empty(e) {
				continue
			}
			g.into(e, next())
		case *ast.NumberLit:
			g.into(e, next())
		default:
			g.into(e, next())
			if _, ok := numberLit(e); !ok {
				convert(adds[i-1].Start)
			}
		}
		part()
	}
	d := dst
	if d < 0 {
		d = w
	}
	if k > 1 {
		g.at(int(n.Start))
		g.emit(bytecode.MakeR(bytecode.RConcat, 0, r16(d), r16(w), uint16(k)))
	} else if d != w {
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(d), r16(w), 0))
	}
	g.top = w
	if dst < 0 {
		g.top = w + 1
	}
	return d, true
}

// isArguments reports whether e is the function's own arguments binding.
func (g *gen) isArguments(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok || id.Name != "arguments" {
		return false
	}
	res, ok := g.rec.Read[id]
	return ok && (res.Kind == ResLocal || res.Kind == ResLocalCheck) && int(res.Index) < len(g.fn.Locals) &&
		g.fn.Locals[res.Index].Name == "arguments"
}

// applyArgs compiles f.apply(x, arguments) of a function whose arguments
// object is never made: f, what f.apply reads and x in three registers,
// and apply_args, which hands on the frame's arguments as passed, R[C]
// the arguments binding's register.
func (g *gen) applyArgs(n *ast.Call, m *ast.Member, dst int) int {
	if _, ok := m.Object.(*ast.Super); ok {
		g.fail("super")
		return g.dest(dst)
	}
	w := g.allocN(3)
	if code := g.fn.Code; g.fn.Leaf == bytecode.LeafForward {
		// this.k.apply(this, arguments), the whole body: a construction
		// answers it without a frame (leafForward), from the caches of the
		// stack form's two reads, which the reads here share so that a run
		// of the body fills them.
		g.at(m.Object.Pos())
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(w), regThis, g.stackSite(code[1])))
		g.at(m.Property.Pos())
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(w+1), r16(w), g.stackSite(code[2])))
		g.emit(bytecode.MakeR(bytecode.RMove, 0, r16(w+2), regThis, 0))
	} else {
		g.into(m.Object, w)
		g.at(m.Property.Pos())
		g.emit(bytecode.MakeR(bytecode.RGetProp, 0, r16(w+1), r16(w), g.propSite(bytecode.OpGetProp, g.nameIdx("apply"))))
		g.into(n.Args[0], w+2)
	}
	d := dst
	if d < 0 {
		d = w
	}
	// The arguments binding's register, undefined until an apply other
	// than the built-in is given the object, which is kept there so that
	// every such call is given the same one.
	g.at(n.TracePos())
	g.emit(bytecode.MakeR(bytecode.RApplyArgs, 0, r16(d), r16(w), r16(int(g.slotReg[g.rec.Read[n.Args[1].(*ast.Ident)].Index]))))
	return d
}

// aux gives the instruction at pc its extra operand (RegCode.Aux).
// debugMark is the cold debug of a place code compiled for a debugger can
// stop at pos, where the stack form made one (Record.Debugs): at each
// statement, loop test and update, and for-in and for-of head.
func (g *gen) debugMark(pos int) {
	m, ok := g.rec.Debugs[int32(pos)]
	if !ok {
		return
	}
	g.at(pos)
	b := 0
	if m.Debugger {
		b = 1
	}
	g.aux(g.cold(bytecode.RDebug, 0, b, 0), m.Index)
}

func (g *gen) aux(pc int, v uint32) {
	if g.auxes == nil {
		g.auxes = map[int]uint32{}
	}
	g.auxes[pc] = v
}

// callSite is a call's cache: the function it called last, and how.
//
// Past 16 bits, the function is compiled again with its sites shared
// (errSites): the calls of one operation share one, whose entry is
// checked against the callee it is made for.
func (g *gen) callSite(op bytecode.ROp) uint16 {
	k := siteKey{kind: "call " + op.String()}
	if g.shareSites {
		if s, ok := g.shared[k]; ok {
			return s
		}
	}
	s := g.calls
	g.calls++
	if s >= limit16 {
		g.fail(errSites)
		return 0
	}
	if g.shareSites {
		if g.shared == nil {
			g.shared = map[siteKey]uint16{}
		}
		g.shared[k] = uint16(s)
	}
	return uint16(s)
}

func (g *gen) newExpr(n *ast.New, dst int) int {
	if wideArgs(n.Args) {
		w := g.allocN(3)
		g.into(n.Callee, w)
		g.spreadArray(n.Args, w+2)
		d := g.dest(dst)
		g.at(n.Start)
		g.emit(bytecode.MakeR(bytecode.RCallSpread, 0, r16(w), r16(d), 1))
		return d
	}
	argc := len(n.Args)
	w := g.allocN(2 + argc)
	g.into(n.Callee, w)
	g.args(n.Args, w+2)
	d := dst
	if d < 0 {
		d = w
	}
	g.at(n.Start)
	g.emit(bytecode.MakeR(bytecode.RNewW, uint8(argc), r16(w), r16(d), 0))
	return d
}

func (g *gen) object(n *ast.ObjectLit, dst int) int {
	if lit := g.objectAtOnce(n, dst); lit >= 0 {
		return lit
	}
	d := g.dest(dst)
	g.at(n.Start)
	// B is the room the literal asks for, a hint.
	g.emit(bytecode.MakeR(bytecode.RNewObject, 0, r16(d), uint16(min(len(n.Props), limit16-1)), g.site16(siteKey{kind: "new_object"})))
	for _, p := range n.Props {
		if g.err != "" {
			return d
		}
		mark := g.top
		g.property(d, p)
		g.top = mark
	}
	return d
}

// objectAtOnce compiles a literal of plain keys and values, none twice and
// no __proto__, of at most 255, as new_object_lit: its values into
// registers in order, then the object made of them at once, rather than a
// define_field each. It is -1 for any other literal, nothing emitted.
func (g *gen) objectAtOnce(n *ast.ObjectLit, dst int) int {
	if len(n.Props) == 0 || len(n.Props) > 255 {
		return -1
	}
	keys := make([]uint32, 0, len(n.Props))
	seen := map[string]bool{}
	for _, p := range n.Props {
		if p.Kind != ast.PropInit || p.Computed || p.Method {
			return -1
		}
		var key string
		switch k := p.Key.(type) {
		case *ast.Ident:
			key = k.Name
		case *ast.StringLit:
			key = k.Value
		default:
			return -1
		}
		if key == "__proto__" || seen[key] {
			return -1
		}
		seen[key] = true
	}
	base := g.allocN(len(n.Props))
	mark := g.top
	for i, p := range n.Props {
		var key string
		switch k := p.Key.(type) {
		case *ast.Ident:
			key = k.Name
		case *ast.StringLit:
			key = k.Value
		}
		keys = append(keys, g.nameIdx(key))
		g.into(p.Value, base+i)
		g.top = mark
	}
	g.objKeys = append(g.objKeys, keys)
	d := g.dest(dst)
	g.at(n.Start)
	at := g.emit(bytecode.MakeR(bytecode.RNewObjectLit, uint8(len(n.Props)), r16(d), r16(base), g.site16(siteKey{kind: "object_lit", keys: fmt.Sprint(keys)})))
	g.aux(at, uint32(len(g.objKeys)-1))
	return d
}

func (g *gen) array(n *ast.ArrayLit, dst int) int {
	if elems, ok := g.constantElements(n.Elements); ok {
		// A literal of constants: its arrays share them until one is
		// written, as the stack form's do.
		g.literals = append(g.literals, elems)
		d := g.dest(dst)
		// B is the count, or 0 past 16 bits: the literal's.
		count := uint16(len(elems))
		if len(elems) >= limit16 {
			count = 0
		}
		at := g.emit(bytecode.MakeR(bytecode.RNewArrayK, 0, r16(d), count, g.site16(siteKey{kind: "array_k", keys: fmt.Sprint(elems)})))
		g.aux(at, uint32(len(g.literals)-1))
		return d
	}
	if len(n.Elements) == 0 {
		if g.arrays >= limit16 {
			// Past 16 bits of empty arrays' sites: one made with none.
			d := g.dest(dst)
			g.emit(bytecode.MakeR(bytecode.RNewArray, 0, r16(d), r16(d), 0))
			return d
		}
		d := g.dest(dst)
		g.emit(bytecode.MakeR(bytecode.RNewArrayE, 0, r16(d), uint16(g.arrays), 0))
		g.arrays++
		return d
	}
	if hasSpread(n.Elements) || len(n.Elements) > arrayChunk {
		return g.spreadArray(n.Elements, dst)
	}
	base := g.allocN(len(n.Elements))
	mark := g.top
	for i, e := range n.Elements {
		if e == nil {
			// A hole: the length grows by one, with no element made.
			g.emit(bytecode.MakeR(bytecode.RLoadUninit, 0, r16(base+i), 0, 0))
			continue
		}
		g.into(e, base+i)
		// What computing it took, given back: each element's an object
		// literal's of its own values had taken them all at once.
		g.top = mark
	}
	d := g.dest(dst)
	g.emit(bytecode.MakeR(bytecode.RNewArray, 0, r16(d), r16(base), uint16(len(n.Elements))))
	return d
}

// constantElements is a literal's elements as constants, where each is a
// number, a negated number, a string, a boolean, null or undefined.
func (g *gen) constantElements(els []ast.Expr) ([]bytecode.LitElem, bool) {
	if len(els) == 0 {
		return nil, false
	}
	out := make([]bytecode.LitElem, len(els))
	for i, e := range els {
		switch v := e.(type) {
		case *ast.NumberLit, *ast.Unary:
			n, ok := numberLit(v)
			if !ok {
				return nil, false
			}
			out[i] = bytecode.LitElem{K: g.constIdx(bytecode.Constant{Kind: bytecode.ConstNumber, Num: n})}
		case *ast.StringLit:
			out[i] = bytecode.LitElem{K: g.constIdx(bytecode.Constant{Kind: bytecode.ConstString, Str: v.Value})}
		case *ast.BoolLit:
			out[i] = bytecode.LitElem{Kind: bytecode.LitFalse}
			if v.Value {
				out[i].Kind = bytecode.LitTrue
			}
		case *ast.NullLit:
			out[i] = bytecode.LitElem{Kind: bytecode.LitNull}
		case *ast.Ident:
			if res, ok := g.rec.Read[v]; !ok || res.Kind != ResLiteral || res.Lit != nil {
				return nil, false
			}
			out[i] = bytecode.LitElem{Kind: bytecode.LitUndefined}
		default:
			return nil, false
		}
	}
	return out, true
}

// closure makes a closure of the nested function lit into register d.
func (g *gen) closure(lit *ast.FuncLit, d int) {
	k := -1
	for i, c := range g.fn.Constants {
		if c.Kind == bytecode.ConstFunction && c.Fn != nil && int(c.Fn.Start) == lit.Start {
			k = i
			break
		}
	}
	if k < 0 {
		g.fail("a function the stack form has no template of")
		return
	}
	g.closureConst(k, d)
}

// closureConst makes a closure of the function of constant k into
// register d.
func (g *gen) closureConst(k, d int) {
	if k < 0 || k >= len(g.fn.Constants) || g.fn.Constants[k].Kind != bytecode.ConstFunction || g.fn.Constants[k].Fn == nil {
		g.fail("a function the stack form has no template of")
		return
	}
	tmpl := g.fn.Constants[k].Fn
	caps := make([]uint16, len(tmpl.Upvalues))
	for i, u := range tmpl.Upvalues {
		if !u.FromParent {
			continue
		}
		r := g.slotReg[u.Index]
		if r < 0 {
			g.fail("a capture of a binding without a register")
			return
		}
		caps[i] = uint16(r)
	}
	g.caps[k] = caps
	g.emit(bytecode.MakeRBC(bytecode.RClosureW, 0, r16(d), uint32(k)))
}

// --- the result -----------------------------------------------------------

func (g *gen) finish() *bytecode.RegCode {
	// Each instruction's position, as the stack form's instruction at the
	// same place: what a frame's pc names for a stack trace.
	type entry struct {
		pos int32
		pc  uint32
	}
	var table []entry
	for i, l := range g.fn.Lines {
		end := uint32(len(g.fn.Code))
		if i+1 < len(g.fn.Lines) {
			end = g.fn.Lines[i+1].PC
		}
		if l.PC < end {
			table = append(table, entry{l.Pos, l.PC})
		}
	}
	sort.SliceStable(table, func(i, j int) bool { return table[i].pos < table[j].pos })
	src := make([]uint32, len(g.code))
	for i, p := range g.pos {
		j := sort.Search(len(table), func(j int) bool { return table[j].pos > p }) - 1
		if j >= 0 && int(table[j].pc) < len(g.fn.Code) {
			src[i] = table[j].pc
		}
	}
	var switches [][]bytecode.SwitchCase
	for _, t := range g.switches {
		cases := make([]bytecode.SwitchCase, len(t))
		for i, e := range t {
			cases[i] = e.c
			cases[i].To = uint32(e.body.at)
		}
		switches = append(switches, cases)
	}
	for at, p := range g.stores {
		j := sort.Search(len(table), func(j int) bool { return table[j].pos > p }) - 1
		if j >= 0 && int(table[j].pc) < len(g.fn.Code) {
			g.aux(at, table[j].pc)
		} else {
			g.aux(at, src[at])
		}
	}
	for len(g.siteName) <= int(g.fn.PropSites) {
		g.siteName = append(g.siteName, 0)
	}
	var marks []uint8
	if len(g.marks) > 0 {
		marks = make([]uint8, len(g.code))
		for pc, m := range g.marks {
			marks[pc] = m
		}
	}
	// A far binding's k made its register, past every near one.
	for _, pc := range g.farPCs {
		g.code[pc] = g.code[pc].WithBC(uint32(g.max+1) + g.code[pc].BC())
	}
	var farNames []uint32
	if len(g.farNames) > 0 {
		farNames = make([]uint32, len(g.code))
		for pc, v := range g.farNames {
			farNames[pc] = v
		}
	}
	var aux []uint32
	if len(g.auxes) > 0 {
		aux = make([]uint32, len(g.code))
		for pc, v := range g.auxes {
			aux[pc] = v
		}
	}
	rc := &bytecode.RegCode{
		Aux: aux, FarNames: farNames, Marks: marks, Switches: switches, Literals: g.literals, ObjKeys: g.objKeys, UsesArguments: (g.fn.UsesArguments || g.fn.MakesArguments) && !g.fn.ForwardsArguments,
		ForwardsArguments: g.fn.ForwardsArguments,
		Code:              g.code, NumRegs: g.max + 1 + g.nfar, SrcPC: src, SiteName: g.siteName, This: -1,
		LocalReg: g.localReg(),
		Wide:     true, Params: g.fn.ParamCount, Inits: g.inits, Caps: g.caps, CallSites: g.calls,
		Handlers: g.handlerTable(), BackLive: g.backLive, KeepsArgs: g.fn.HasRest || g.forwardsArgs, ThisRef: g.thisRef,
	}
	if why := Verify(rc, g.fn); why != "" {
		Refuse(g.orig, why)
		return nil
	}
	return rc
}

// localReg is LocalReg: each local's register, where the code has a
// call_reuse or a direct_eval, and nil otherwise.
func (g *gen) localReg() []int32 {
	if !g.reuses && len(g.rec.Evals) == 0 && g.fn.Debug == nil {
		return nil
	}
	regs := append([]int32(nil), g.slotReg...)
	for i, r := range regs {
		if r >= farReg {
			// What reads a local by its register reads a near one: a far
			// binding has none it can (farSlots keeps every binding a
			// direct eval or a debugger reaches near).
			regs[i] = -1
		}
	}
	return regs
}

// capturedRegs is the registers of the bindings a lexical declaration
// declares that a closure keeps.
func (g *gen) capturedRegs(d *ast.VarDecl) []int {
	var ids []*ast.Ident
	for _, x := range d.Decls {
		ids = patternNames(x.Target, ids)
	}
	var regs []int
	for _, id := range ids {
		if res, ok := g.rec.Write[id]; ok && res.Kind == ResLocalInit && g.fn.Locals[res.Index].Captured && g.slotReg[res.Index] >= 0 {
			regs = append(regs, int(g.slotReg[res.Index]))
		}
	}
	return regs
}

// closeRegs closes the captures open on each of regs (close_regs): the
// closures that keep them keep the values they have, and the next closure
// made opens new ones.
func (g *gen) closeRegs(regs []int) {
	for _, r := range regs {
		g.cold(bytecode.RCloseRegs, r, 1, 0)
	}
}
