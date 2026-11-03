//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"fmt"
	"slices"

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
// asks it as it asks jitFeedback (zzjit_snapshot_gen.go); a question it
// was not asked ahead is a miss (jitSnapMiss), which has the compile built
// on the runtime's goroutine after all. It reads nothing of the runtime's:
// answers are values, and addresses the VM keeps alive with the compile,
// as the feedback's own are.
type jitSnapshot struct {
	// answers are the questions answered with something: one of a PC,
	// asked at every PC of the program's codeLen, that answered nothing --
	// not ok, or no calls -- is not kept, and answers nothing again.
	answers map[jitSnapKey][]any
	codeLen int
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
	// the asking took back: they are the build's only where it asks the
	// question, as often as it does (uses, the root's, in the order the
	// build asked them), and the install puts them back so (replay). bad,
	// the root's, marks a question whose lists shrank, which the capture
	// cannot take back: such a compile is built on the runtime's goroutine.
	fb      *jitFeedback
	effects map[jitSnapKey]*jitSnapEffect
	uses    []jitSnapUse
	bad     bool
}

// jitSnapLists are what a question may append to of a feedback's: what the
// compile keeps alive with its code, and the calls, callees and closures it
// decides, which its install takes into its entry.
type jitSnapLists struct {
	shapes                        []*shape
	holders                       []*Object
	cells                         []*Value
	links                         []*jitLink
	inlined, made, wanted, linked []*closure
	pools                         []*abi.ObjectPool
}

// jitSnapLens are the lengths of a feedback's lists (jitSnapLists), all a
// question's effect is found by: most questions append nothing.
type jitSnapLens [9]int

func jitSnapLensOf(f *jitFeedback) jitSnapLens {
	return jitSnapLens{len(f.shapes), len(f.holders), len(f.cells), len(f.links), len(f.inlined), len(f.made), len(f.wanted), len(f.linked), len(f.pools)}
}

// jitSnapEffect is what a question appended to the asked feedback's lists
// (own) and to its keep's (kept), where that is another.
type jitSnapEffect struct {
	own, kept jitSnapLists
}

// jitSnapUse is a question the build asked, of a snapshot.
type jitSnapUse struct {
	s   *jitSnapshot
	key jitSnapKey
}

// jitSnapMethod is a method of jitFeedback's, numbered (jitSnapNames).
type jitSnapMethod uint8

// jitSnapKey is a question: its method and its argument, a PC mostly.
type jitSnapKey struct {
	method jitSnapMethod
	arg    int64
}

// jitSnapCallee is what makes a callee's feedback answer as it does: its
// closure, its function, the forwarding frames it is inlined at, and the
// feedback it keeps what it holds in.
type jitSnapCallee struct {
	cl           *closure
	fn           any
	fwd, fwdArgs *jitForwardFrame
	root         *jitFeedback
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

// get is the answer to a question; nil for one of a PC, asked there, that
// answered nothing, which its method answers with its results' zero
// values; or a miss, which the worker's build recovers (jitJob.run): a
// question of a PC whose op the method is not asked at (jitSnapOpMethods)
// is a miss too.
func (s *jitSnapshot) get(m jitSnapMethod, arg int64) []any {
	key := jitSnapKey{m, arg}
	if _, ok := s.effects[key]; ok {
		s.root.uses = append(s.root.uses, jitSnapUse{s, key})
	}
	a, ok := s.answers[key]
	if !ok {
		if jitSnapAtPC[m] && arg >= 0 && arg < int64(s.codeLen) && jitSnapAsks[s.ops[arg]]&(1<<m) != 0 {
			return nil
		}
		panic(jitSnapMiss{m, arg, s.depth})
	}
	return a
}

// jitSnapAnswered reports whether a question of a PC answered something,
// by its last result: ok, or a list with something in it.
func jitSnapAnswered(last any) bool {
	switch x := last.(type) {
	case bool:
		return x
	case []ssa.GenStep:
		return len(x) != 0
	case []ssa.CallSite:
		return len(x) != 0
	}
	return true
}

// put keeps an answer.
func (s *jitSnapshot) put(m jitSnapMethod, arg int64, results ...any) {
	s.answers[jitSnapKey{m, arg}] = results
}

// begin is the lengths of the lists of fb and of its keep, before a
// question, which end compares with.
func (s *jitSnapshot) begin(fb *jitFeedback) [2]jitSnapLens {
	return [2]jitSnapLens{jitSnapLensOf(fb), jitSnapLensOf(fb.keep())}
}

// end takes back what the question key appended to fb's lists and its
// keep's since begin, keeping it as the question's effect. A question that
// changed no length -- most of them -- has none, and its lists are left as
// they are: their slices written back, each a pointer stored through the
// collector's write barrier, the capture had cost two fifths of the
// snapshot's time.
func (s *jitSnapshot) end(fb *jitFeedback, before [2]jitSnapLens, m jitSnapMethod, arg int64) {
	k := fb.keep()
	ownMoved := jitSnapLensOf(fb) != before[0]
	keptMoved := k != fb && jitSnapLensOf(k) != before[1]
	if !ownMoved && !keptMoved {
		return
	}
	var own, kept jitSnapLists
	if ownMoved {
		own = jitSnapTake(fb, before[0], &s.root.bad)
	}
	if keptMoved {
		kept = jitSnapTake(k, before[1], &s.root.bad)
	}
	if own.empty() && kept.empty() {
		return
	}
	s.effects[jitSnapKey{m, arg}] = &jitSnapEffect{own: own, kept: kept}
}

// jitSnapTake is what f's lists gained past the lengths before, copied, and
// f's lists cut back to them; a list shorter than it was sets bad.
func jitSnapTake(f *jitFeedback, before jitSnapLens, bad *bool) jitSnapLists {
	var d jitSnapLists
	d.shapes, f.shapes = jitSnapGained(f.shapes, before[0], bad)
	d.holders, f.holders = jitSnapGained(f.holders, before[1], bad)
	d.cells, f.cells = jitSnapGained(f.cells, before[2], bad)
	d.links, f.links = jitSnapGained(f.links, before[3], bad)
	d.inlined, f.inlined = jitSnapGained(f.inlined, before[4], bad)
	d.made, f.made = jitSnapGained(f.made, before[5], bad)
	d.wanted, f.wanted = jitSnapGained(f.wanted, before[6], bad)
	d.linked, f.linked = jitSnapGained(f.linked, before[7], bad)
	d.pools, f.pools = jitSnapGained(f.pools, before[8], bad)
	return d
}

// jitSnapGained is what now has past its first n, copied, and now cut back
// to n.
func jitSnapGained[T any](now []T, n int, bad *bool) (gained, back []T) {
	if len(now) < n {
		*bad = true
		return nil, now
	}
	if len(now) == n {
		return nil, now
	}
	return slices.Clone(now[n:]), now[:n]
}

func (l *jitSnapLists) empty() bool {
	return len(l.shapes)+len(l.holders)+len(l.cells)+len(l.links)+len(l.inlined)+len(l.made)+len(l.wanted)+len(l.linked)+len(l.pools) == 0
}

// apply appends l's lists to f's, as the feedback appends to them: the
// callees it wants compiled and those it calls through their links once
// each, a closure it does not make once, with its inlining (each guarded
// by slices.Contains in jitFeedback's), everything else as often as a
// question appends it.
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
	f.wanted = jitSnapAppendNew(f.wanted, l.wanted)
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
		e := u.s.effects[u.key]
		e.own.apply(u.s.fb)
		if k := u.s.fb.keep(); k != u.s.fb {
			e.kept.apply(k)
		}
	}
}

// jitSnapCopy is a slice an answer holds, copied: jitFeedback makes its
// answers anew at each question, and a build may change what it is given.
func jitSnapCopy[T any](s []T) []T { return slices.Clone(s) }

// jitSnapshotOf is fb's snapshot for p, its function's program: every
// question asked ahead.
func jitSnapshotOf(fb *jitFeedback, p *ir.Program) *jitSnapshot {
	s := &jitSnapshot{answers: map[jitSnapKey][]any{}, callees: map[jitSnapCallee]*jitSnapshot{}, fb: fb, effects: map[jitSnapKey]*jitSnapEffect{}}
	s.root = s
	s.ask(fb, p)
	// The questions of other arguments, of what the answers name: the
	// leaves whose kinds the build asks (LeafKind), and the pools whose
	// objects escape analysis asks how Go makes (VirtualRoot), in the
	// root's feedback, which the build asks them of.
	fns, pools := map[uintptr]bool{}, map[uintptr]bool{}
	for _, c := range append([]*jitSnapshot{s}, mapValues(s.callees)...) {
		for k, a := range c.answers {
			if k.method == snapLeaf {
				if fn, ok := a[0].(uintptr); ok && fn != 0 {
					fns[fn] = true
				}
			}
			for _, x := range a {
				jitSnapPools(x, pools)
			}
		}
	}
	for fn := range fns {
		k := fb.LeafKind(fn)
		s.put(snapLeafKind, int64(fn), k)
		for _, c := range s.callees {
			c.put(snapLeafKind, int64(fn), k)
		}
	}
	for pool := range pools {
		shape, proto, ok := fb.VirtualRoot(pool)
		s.put(snapVirtualRoot, int64(pool), shape, proto, ok)
	}
	return s
}

// ask asks fb every question of p's: at each PC, the methods of its op.
func (s *jitSnapshot) ask(fb *jitFeedback, p *ir.Program) {
	s.codeLen = len(p.Code)
	s.ops = make([]ir.Op, len(p.Code))
	s.askOnce(fb)
	for pc := range p.Code {
		op := p.Code[pc].Op
		s.ops[pc] = op
		for m := jitSnapMethod(0); int(m) < len(jitSnapAskers); m++ {
			if jitSnapAsks[op]&(1<<m) != 0 {
				jitSnapAskers[m](s, fb, pc)
			}
		}
	}
}

// jitSnapOps are the ops the build asks each method of a PC at, as the
// VM's tests and the ssr and V8 workloads have it ask them, plain and
// under QJS_JIT_STRESS (QJS_JIT_SNAPOPS writes what a run asked): asked at
// any other op, a question misses, and the compile is built on the
// runtime's goroutine (jitSnapMiss). Generic, EntryGeneric and Resumed,
// asked at most ops, are asked at every one.
var jitSnapOps = map[jitSnapMethod][]ir.Op{
	snapNativeCalls:    {ir.Host, ir.PropertyRead, ir.PropertyWrite, ir.BindingRead, ir.ReferenceRead, ir.BindingWrite, ir.FieldDefine, ir.Rethrow},
	snapBigInt:         {ir.Binary, ir.Unary, ir.Branch, ir.StringAdd},
	snapFrequency:      {ir.Host, ir.PropertyRead, ir.PropertyWrite, ir.ReferenceRead},
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
	snapInlineExit:     {ir.Host},
	snapParseInt:       {ir.Host},
	snapStringProperty: {ir.ReferenceRead},
	snapGenSteps:       {ir.IterStep},
	snapKeyedRead:      {ir.ArrayRead},
	snapBigIntFailed:   {ir.Binary},
	snapSubstring:      {ir.Host},
	snapKeyedWrite:     {ir.ArrayWrite},
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
}

// jitSnapAsks is jitSnapOps by op: a bit for each method asked there.
var jitSnapAsks = func() (t [ir.ParamDefault + 1]uint64) {
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

// nest is site with its callee's feedback a snapshot, asked ahead, once
// for each callee's feedback the root has met; past jitSnapDepth, one that
// answers nothing.
func (s *jitSnapshot) nest(site ssa.InlineSite) ssa.InlineSite {
	f, ok := site.Feedback.(*jitFeedback)
	if !ok {
		return site
	}
	root := s.root
	key := jitSnapCallee{cl: f.cl, fn: f.fn, fwd: f.fwd, fwdArgs: f.fwdArgs, root: f.root}
	if c := root.callees[key]; c != nil {
		site.Feedback = c
		return site
	}
	c := &jitSnapshot{answers: map[jitSnapKey][]any{}, root: root, depth: s.depth + 1, fb: f, effects: map[jitSnapKey]*jitSnapEffect{}}
	root.callees[key] = c
	if c.depth < jitSnapDepth && site.Program != nil {
		c.ask(f, site.Program)
	}
	site.Feedback = c
	return site
}

// jitSnapPools adds to pools every pool an answer names: a literal's, a
// closure's, an inline site's, a call's -- an accessor's and a loop's
// among them.
func jitSnapPools(x any, pools map[uintptr]bool) {
	add := func(p uintptr) {
		if p != 0 {
			pools[p] = true
		}
	}
	switch v := x.(type) {
	case ssa.LiteralSite:
		add(v.Pool)
	case ssa.ClosureSite:
		add(v.Pool)
	case ssa.InlineSite:
		add(v.Pool)
	case []ssa.CallSite:
		for i := range v {
			add(v[i].Pool)
		}
	case ssa.AccessorSite:
		if v.Call != nil {
			add(v.Call.Pool)
		}
		if v.Inline != nil {
			add(v.Inline.Pool)
		}
	case ssa.ForEachSite:
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

// buildFromSnapshot is the worker's build of j from its snapshot: the
// function, or the build's refusal (buildErr), or a miss (missed).
func (j *jitJob) buildFromSnapshot() (f *ssa.Func, err error) {
	defer func() {
		if v := recover(); v != nil {
			m, ok := v.(jitSnapMiss)
			if !ok {
				panic(v)
			}
			j.missed, f, err = true, nil, m
		}
	}()
	f, err = jitBuildFrom(j.w, j.fn, j.p, j.fb, j.snap, j.timing)
	if err != nil {
		j.buildErr = err
	}
	return f, err
}
