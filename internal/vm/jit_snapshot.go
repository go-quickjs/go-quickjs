//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ir"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// A jitSnapshot is a build's feedback asked ahead, on the runtime's
// goroutine, so that the build itself may be made on a worker (D8, phase
// 5), as V8's serializer for background compilation copied what TurboFan
// would read into its JSHeapBroker: every question of jitFeedback's at
// every PC of the program, the questions of no argument once, and those of
// the callees the answers' inline sites name, to the inliner's depth
// (jitSnapDepth), each callee's feedback a snapshot of its own. The build
// asks it as it asks jitFeedback (jit_snapshot_gen.go); a question it
// was not asked ahead is a miss (jitSnapMiss), which has the compile built
// on the runtime's goroutine after all. It reads nothing of the runtime's:
// answers are values, and addresses the VM keeps alive with the compile,
// as the feedback's own are.
type jitSnapshot struct {
	// a are the questions answered with something, each method's results
	// in a table of its own by argument (jit_snapshot_gen.go): one of a
	// PC, asked at every PC of the program's codeLen, that answered
	// nothing -- not ok, or no calls -- is not kept, and answers nothing
	// again. Kept boxed in a map by method and argument, the answers'
	// keeping had been about half of the snapshot's time.
	a       jitSnapAnswers
	codeLen int
	// room is how many PCs ask each method, the most its table holds.
	room [len(jitSnapNames)]int32
	// ops are the ops of the program's instructions, by PC.
	ops []ir.Op
	// root is the snapshot of the compile's own function, whose callees'
	// snapshots it keeps by their feedback's identity (callees), and depth
	// how deep in callees this one is.
	root    *jitSnapshot
	callees map[jitSnapCallee]*jitSnapshot
	depth   int
	// fb is the feedback this one was asked of, and effects what each
	// question appended to its lists and its keep's (jitSnapLists), which
	// the asking took back, each question's by its index past 0 (its
	// answer's eff): they are the build's only where it asks the question,
	// as often as it does (uses, the root's, in the order the build asked
	// them), and the install puts them back so (replay). gained, the
	// root's, holds what the effects took back, and spans, the root's,
	// the runs of it each effect is. bad, the root's, marks a question
	// whose lists shrank, which the capture cannot take back: such a
	// compile is built on the runtime's goroutine.
	fb      *jitFeedback
	effects []jitSnapEffect
	gained  jitSnapLists
	spans   []jitSnapSpan
	uses    []jitSnapUse
	bad     bool
	// reg is the snapshot as register code's feedback answers, where it
	// was asked of that (jit_snap.go).
	reg *jitRegSnapshot
	// d is what the gathers copied for the answers the worker derives,
	// and sink the feedback their effects are appended to
	// (jit_snapderive.go).
	d    jitSnapData
	sink *jitFeedback
	// rf is the register feedback askReg asked, nil where none was: the
	// gathers' and the runtime goroutine's, never read by a derivation.
	// rc is its register code, which the gathers are keyed by, and p the
	// program lowered of it, which the questions are asked by: bound once
	// it is lowered (bindProgram), on the goroutine or on the worker.
	rf *jitRegFeedback
	rc *bytecode.RegCode
	p  *ir.Program
}

// jitSnapLists are what a question may append to of a feedback's: what the
// compile keeps alive with its code, and the calls, callees and closures it
// decides, which its install takes into its entry.
type jitSnapLists struct {
	shapes                []*shape
	holders               []*Object
	cells                 []*Value
	links                 []*jitLink
	inlined, made, linked []*closure
	pools                 []*abi.ObjectPool
}

// jitSnapDerivedLinks is the span list of the links a derivation's
// effects name, kept apart from gained's (jitSnapData's derivedLinks).
const jitSnapDerivedLinks = 8

// jitSnapLens are the lengths of a feedback's lists (jitSnapLists), all a
// question's effect is found by: most questions append nothing.
type jitSnapLens [8]int

func jitSnapLensOf(f *jitFeedback) jitSnapLens {
	return jitSnapLens{len(f.shapes), len(f.holders), len(f.cells), len(f.links), len(f.inlined), len(f.made), len(f.linked), len(f.pools)}
}

// jitSnapEffect is what a question appended to the asked feedback's lists
// and to its keep's, where that is another: the runs of the root's spans
// from its from to its to, each a run of the root's gained lists.
type jitSnapEffect struct {
	from, to int32
}

// jitSnapSpan is a run of a list a question appended to (list, a list of
// jitSnapLists by its index in jitSnapLens, or jitSnapDerivedLinks; kept,
// of the keep's, not the asked feedback's own), from:to of the root's
// gained list of it.
type jitSnapSpan struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_        [0]func()
	list     uint8
	kept     bool
	from, to int32
}

// jitSnapSame reports whether two feedbacks' lengths are the same, as ==
// would, without the call of the runtime's comparison of memory it makes
// of an array this long: it had been a tenth of the snapshot's time.
func jitSnapSame(a, b *jitSnapLens) bool {
	return a[0] == b[0] && a[1] == b[1] && a[2] == b[2] && a[3] == b[3] &&
		a[4] == b[4] && a[5] == b[5] && a[6] == b[6] && a[7] == b[7]
}

// jitSnapUse is a question the build asked of a snapshot that had an
// effect: the effect's index past 0 (jitSnapEntry's eff).
type jitSnapUse struct {
	s   *jitSnapshot
	eff int32
}

// jitSnapMethod is a method of jitFeedback's, numbered (jitSnapNames).
type jitSnapMethod uint8

// jitSnapEntry is a method's answer to a question, as its snapshot keeps
// it: the question's argument, a PC mostly; its effect's index past 0, or
// 0 for none; and its results, v, if it answered something -- a question
// of a PC that answered nothing is kept only for its effect.
type jitSnapEntry[T any] struct {
	// Nothing compares it: comparable, the type had an equality function
	// the compiler lays out among package vm's code.
	_        [0]func()
	arg      int64
	eff      int32
	answered bool
	v        T
}

// jitSnapTable is a method's answers, by argument: put in the order they
// were asked, their PCs' mostly, which unsorted marks they were not, until
// seal sorts them.
type jitSnapTable[T any] struct {
	e        []jitSnapEntry[T]
	unsorted bool
}

// put keeps an answer to the question of arg as a map by question would:
// a later answer to the same question replacing an earlier and a later
// effect an earlier, and the question asked again with no answer, or no
// effect, leaving what was kept of it.
func (t *jitSnapTable[T]) put(arg int64, eff int32, answered bool, v T) {
	if n := len(t.e); n > 0 {
		if last := &t.e[n-1]; last.arg == arg {
			last.merge(eff, answered, v)
			return
		} else if arg < last.arg {
			t.unsorted = true
		}
	}
	t.e = append(t.e, jitSnapEntry[T]{arg: arg, eff: eff, answered: answered, v: v})
}

// merge is e's question asked again, as put keeps it.
func (e *jitSnapEntry[T]) merge(eff int32, answered bool, v T) {
	if answered {
		e.answered, e.v = true, v
	}
	if eff != 0 {
		e.eff = eff
	}
}

// seal sorts t's answers by argument where they were put out of that
// order, those to one question merged in the order they were put.
func (t *jitSnapTable[T]) seal() {
	if !t.unsorted {
		return
	}
	t.unsorted = false
	slices.SortStableFunc(t.e, func(a, b jitSnapEntry[T]) int { return cmp.Compare(a.arg, b.arg) })
	out := t.e[:0]
	for _, e := range t.e {
		if n := len(out); n > 0 && out[n-1].arg == e.arg {
			out[n-1].merge(e.eff, e.answered, e.v)
			continue
		}
		out = append(out, e)
	}
	clear(t.e[len(out):])
	t.e = out
}

// reserve makes t room for n answers at its first, the most its method
// may answer: the PCs it is asked at. A table grown an answer at a time
// had copied its answers each time it grew, a property site's two
// hundred bytes, a fifth of what the snapshot allocated.
func (t *jitSnapTable[T]) reserve(n int) {
	if t.e == nil && n > 1 {
		t.e = make([]jitSnapEntry[T], 0, n)
	}
}

// find is the answer to the question of arg, nil for none.
func (t *jitSnapTable[T]) find(arg int64) *jitSnapEntry[T] {
	i, ok := slices.BinarySearchFunc(t.e, arg, func(e jitSnapEntry[T], arg int64) int { return cmp.Compare(e.arg, arg) })
	if !ok {
		return nil
	}
	return &t.e[i]
}

// answered counts the questions t answered.
func (t *jitSnapTable[T]) answered() int {
	n := 0
	for i := range t.e {
		if t.e[i].answered {
			n++
		}
	}
	return n
}

// jitSnapCallee is what makes a callee's feedback answer as it does: its
// closure, its function, the forwarding frames it is inlined at, and the
// feedback it keeps what it holds in.
type jitSnapCallee struct {
	cl   *closure
	fn   any
	root *jitFeedback
}

// jitSnapDepth is how deep in callees the snapshot asks: past ssa's
// maxInlineDepth, which the builder inlines to, so that every callee the
// build may inline has its answers; a question of a callee deeper is a
// miss (jitSnapMiss), the compile built on the runtime's goroutine at its
// install.
const jitSnapDepth = 6

// jitSnapMiss is a build's question its snapshot was not asked ahead.
type jitSnapMiss struct {
	method jitSnapMethod
	arg    int64
	depth  int
}

func (m jitSnapMiss) Error() string {
	return fmt.Sprintf("jit: the snapshot has no %s(%d), %d callees deep", jitSnapNames[m.method], m.arg, m.depth)
}

// absent is the answer to a question the snapshot kept no answer to: none,
// for one of a PC asked there, which its method answers with its results'
// zero values; or a miss, which the worker's build recovers
// (jitRegJob.run): a question of a PC whose op the method is not asked at
// (jitSnapOps) is a miss too.
func (s *jitSnapshot) absent(m jitSnapMethod, arg int64) {
	if jitSnapAtPC[m] && arg >= 0 && arg < int64(s.codeLen) && jitSnapAsks[s.ops[arg]]&(1<<m) != 0 {
		return
	}
	panic(jitSnapMiss{m, arg, s.depth})
}

// used notes the build's asking of a question whose effect is eff, an
// index past 0, which the install replays.
func (s *jitSnapshot) used(eff int32) {
	s.root.uses = append(s.root.uses, jitSnapUse{s, eff})
}

// begin is the lengths of the lists of fb and of its keep, before a
// question, which end compares with.
func (s *jitSnapshot) begin(fb *jitFeedback) [2]jitSnapLens {
	return [2]jitSnapLens{jitSnapLensOf(fb), jitSnapLensOf(fb.keep())}
}

// end takes back what the question asked since begin appended to fb's
// lists and its keep's, keeping it as the question's effect, whose index
// past 0 it is, or 0 for none. A question that changed no length -- most
// of them -- has none, and its lists are left as they are: their slices
// written back, each a pointer stored through the collector's write
// barrier, the capture had cost two fifths of the snapshot's time.
func (s *jitSnapshot) end(fb *jitFeedback, before [2]jitSnapLens) int32 {
	k := fb.keep()
	now := jitSnapLensOf(fb)
	ownMoved := !jitSnapSame(&now, &before[0])
	keptMoved := false
	if k != fb {
		kn := jitSnapLensOf(k)
		keptMoved = !jitSnapSame(&kn, &before[1])
	}
	if !ownMoved && !keptMoved {
		return 0
	}
	root := s.root
	from := len(root.spans)
	if ownMoved {
		root.take(fb, before[0], false)
	}
	if keptMoved {
		root.take(k, before[1], true)
	}
	if len(root.spans) == from {
		return 0
	}
	s.effects = append(s.effects, jitSnapEffect{int32(from), int32(len(root.spans))})
	return int32(len(s.effects))
}

// take is what f's lists gained past the lengths before, copied to the
// ends of the root's gained lists, a span of each, and f's lists cut back
// to them; a list shorter than it was sets bad.
func (root *jitSnapshot) take(f *jitFeedback, before jitSnapLens, kept bool) {
	g := &root.gained
	f.shapes = jitSnapGained(root, 0, kept, f.shapes, before[0], &g.shapes)
	f.holders = jitSnapGained(root, 1, kept, f.holders, before[1], &g.holders)
	f.cells = jitSnapGained(root, 2, kept, f.cells, before[2], &g.cells)
	if root.d.deriving {
		// A derivation's, on the worker, apart from what holdsLink reads on
		// the runtime's goroutine meanwhile: links its gathers made, which
		// gained holds already (gatherLink).
		f.links = jitSnapGained(root, jitSnapDerivedLinks, kept, f.links, before[3], &root.d.derivedLinks)
	} else {
		f.links = jitSnapGained(root, 3, kept, f.links, before[3], &g.links)
	}
	f.inlined = jitSnapGained(root, 4, kept, f.inlined, before[4], &g.inlined)
	f.made = jitSnapGained(root, 5, kept, f.made, before[5], &g.made)
	f.linked = jitSnapGained(root, 6, kept, f.linked, before[6], &g.linked)
	f.pools = jitSnapGained(root, 7, kept, f.pools, before[7], &g.pools)
}

// jitSnapGained is now cut back to its first n, what it has past them
// copied to the end of into, a span of root's: an effect's lists are runs
// of the snapshot's, not lists of their own, a few allocations a snapshot
// rather than a few a question.
func jitSnapGained[T any](root *jitSnapshot, list uint8, kept bool, now []T, n int, into *[]T) []T {
	if len(now) < n {
		root.bad = true
		return now
	}
	if len(now) == n {
		return now
	}
	from := len(*into)
	*into = append(*into, now[n:]...)
	root.spans = append(root.spans, jitSnapSpan{list: list, kept: kept, from: int32(from), to: int32(len(*into))})
	return now[:n]
}

// lists is e's effect as the lists it appended to the asked feedback's
// (own) and to its keep's (kept), each a slice of the root's gained lists.
func (root *jitSnapshot) lists(e jitSnapEffect) (own, kept jitSnapLists) {
	g := &root.gained
	for _, sp := range root.spans[e.from:e.to] {
		l := &own
		if sp.kept {
			l = &kept
		}
		a, b := sp.from, sp.to
		switch sp.list {
		case 0:
			l.shapes = g.shapes[a:b:b]
		case 1:
			l.holders = g.holders[a:b:b]
		case 2:
			l.cells = g.cells[a:b:b]
		case 3:
			l.links = g.links[a:b:b]
		case jitSnapDerivedLinks:
			l.links = root.d.derivedLinks[a:b:b]
		case 4:
			l.inlined = g.inlined[a:b:b]
		case 5:
			l.made = g.made[a:b:b]
		case 6:
			l.linked = g.linked[a:b:b]
		case 7:
			l.pools = g.pools[a:b:b]
		}
	}
	return own, kept
}

func (l *jitSnapLists) empty() bool {
	return len(l.shapes)+len(l.holders)+len(l.cells)+len(l.links)+len(l.inlined)+len(l.made)+len(l.linked)+len(l.pools) == 0
}

// apply appends l's lists to f's, as the feedback appends to them: the
// callees it calls through their links once each, a closure it does not
// make once, with its inlining (each guarded by slices.Contains in
// jitFeedback's), everything else as often as a question appends it.
func (l *jitSnapLists) apply(f *jitFeedback) {
	f.shapes = append(f.shapes, l.shapes...)
	f.holders = append(f.holders, l.holders...)
	f.cells = append(f.cells, l.cells...)
	f.links = append(f.links, l.links...)
	inlined := l.inlined
	for _, t := range l.made {
		if slices.Contains(f.made, t) {
			// Made already: neither it nor its inlining is appended again.
			if i := slices.Index(inlined, t); i >= 0 {
				inlined = slices.Delete(slices.Clone(inlined), i, i+1)
			}
			continue
		}
		f.made = append(f.made, t)
	}
	f.inlined = append(f.inlined, inlined...)
	f.linked = jitSnapAppendNew(f.linked, l.linked)
	f.pools = append(f.pools, l.pools...)
}

// jitSnapAppendNew appends to list what of add it has not.
func jitSnapAppendNew[T comparable](list, add []T) []T {
	for _, x := range add {
		if !slices.Contains(list, x) {
			list = append(list, x)
		}
	}
	return list
}

// replay puts back, on the runtime's goroutine, what the questions the
// build asked appended, as often and in the order it asked them: the lists
// as the build from the feedback would have left them.
func (s *jitSnapshot) replay() {
	for _, u := range s.uses {
		own, kept := s.lists(u.s.effects[u.eff-1])
		own.apply(u.s.fb)
		if k := u.s.fb.keep(); k != u.s.fb {
			kept.apply(k)
		}
	}
}

// jitSnapCopy is a slice an answer holds, copied: jitFeedback makes its
// answers anew at each question, and a build may change what it is given.
func jitSnapCopy[T any](s []T) []T { return slices.Clone(s) }

// jitSnapOps are the ops the build asks each method of a PC at, as the
// VM's tests and the ssr and V8 workloads have it ask them, plain and
// under QJS_JIT_STRESS: asked at any other op, a question misses, and the
// compile is built on the runtime's goroutine (jitSnapMiss). Generic,
// EntryGeneric and Resumed, asked at most ops, are asked at every one. A
// method of a PC the register feedback has is listed here
// (TestJITSnapshotAsksEveryPCMethod): KeyedStore and ViaLeaf, not, had
// been a miss at every compile that asked them.
//
// It is an array, which the linker lays out as data, not a map: a map
// literal this long is filled by a function the compiler lays out before
// every function of the package, ahead of the layout lock's first pad, A,
// whose room the JIT's compiler, linked before package vm, grows into; its
// 4 KB there had put A's range a page on (docs/optimizing.md, "The layout
// lock").
var jitSnapOps = [...][]ir.Op{
	snapNativeCalls:    {ir.Host, ir.PropertyRead, ir.PropertyWrite, ir.BindingRead, ir.ReferenceRead, ir.BindingWrite, ir.FieldDefine, ir.Rethrow, ir.IterStep},
	snapBigInt:         {ir.Binary, ir.Unary, ir.Branch, ir.StringAdd},
	snapFrequency:      {ir.Host, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead},
	snapOperands:       {ir.Binary, ir.Unary, ir.Branch, ir.Update},
	snapProperty:       {ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead},
	snapAccessor:       {ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead},
	snapGlobal:         {ir.BindingRead, ir.BindingWrite, ir.BindingCheck},
	snapCallShape:      {ir.Host, ir.FieldDefine, ir.ArrayLiteral},
	snapTypedBigRead:   {ir.ArrayRead, ir.ArrayUpdate},
	snapLiteral:        {ir.ObjectLiteral, ir.ArrayLiteral},
	snapBigIntWide:     {ir.Binary, ir.Branch},
	snapInstanceOf:     {ir.Host},
	snapBigString:      {ir.Host},
	snapLeaf:           {ir.Host},
	snapCharCode:       {ir.Host},
	snapIndexOf:        {ir.Host},
	snapClosure:        {ir.Closure},
	snapFrameMade:      {ir.Host},
	snapInlineExit:     {ir.Host},
	snapParseInt:       {ir.Host},
	snapStringProperty: {ir.ReferenceRead},
	snapGenSteps:       {ir.IterStep},
	snapKeyedRead:      {ir.ArrayRead},
	snapArgumentsRead:  {ir.ArrayRead, ir.ArrayLength},
	snapBigIntFailed:   {ir.Binary},
	snapSubstring:      {ir.Host},
	snapKeyedWrite:     {ir.ArrayWrite},
	snapKeyedStore:     {ir.ArrayWrite, ir.Host},
	snapViaLeaf:        {ir.Host},
	snapIntrinsic:      {ir.Host},
	snapFromCharCode:   {ir.Host},
	snapInline:         {ir.Host},
	snapStringCell:     {ir.StringConst},
	snapDefine:         {ir.FieldDefine},
	snapStringContent:  {ir.StringConst},
	snapForEach:        {ir.Host},
	snapCalledClosure:  {ir.Closure},
	snapBigIntCell:     {ir.BigIntConst},
	snapStringRead:     {ir.ArrayRead},
	snapTypedRead:      {ir.ArrayRead},
	snapTypedKind:      {ir.ArrayRead, ir.ArrayUpdate, ir.ArrayWrite},
	snapUpvalueTyped:   {ir.UpvalueRead},
	snapRawMath:        {ir.Raw},
}

// jitSnapAsks is jitSnapOps by op: a bit for each method asked there.
var jitSnapAsks = func() (t [ir.LastOp + 1]uint64) {
	for m, ops := range jitSnapOps {
		for _, op := range ops {
			t[op] |= 1 << m
		}
	}
	for op := range t {
		t[op] |= 1<<snapGeneric | 1<<snapEntryGeneric | 1<<snapResumed
	}
	return t
}()

// jitSnapPools adds to pools every pool an answer's result x names, a
// pointer to the result: a literal's, a closure's, an inline site's, a
// call's -- an accessor's and a loop's among them.
func jitSnapPools(x any, pools *[]uintptr) {
	add := func(p uintptr) {
		if p != 0 {
			*pools = append(*pools, p)
		}
	}
	switch v := x.(type) {
	case *ssa.LiteralSite:
		add(v.Pool)
	case *ssa.ClosureSite:
		add(v.Pool)
	case *ssa.InlineSite:
		add(v.Pool)
	case *[]ssa.CallSite:
		for i := range *v {
			add((*v)[i].Pool)
		}
	case *ssa.AccessorSite:
		if v.Call != nil {
			add(v.Call.Pool)
		}
		if v.Inline != nil {
			add(v.Inline.Pool)
		}
	case *ssa.ForEachSite:
		add(v.Callback.Pool)
		if v.Inline != nil {
			add(v.Inline.Pool)
		}
	}
}

func mapValues[K comparable, V any](m map[K]V) []V {
	out := make([]V, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
