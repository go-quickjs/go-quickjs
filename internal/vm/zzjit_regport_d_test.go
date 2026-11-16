//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
	"github.com/go-quickjs/go-quickjs/internal/jit"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// regEntryNamedForTest is the register code, in r's JIT's cache, of the
// function named name -- a global function, a method or an arrow alike --
// nil where the cache holds none with code: the register form's
// counterpart of the stack form's entry found in r.jit.cache by its
// function's name.
func regEntryNamedForTest(r *Runtime, name string) *jitRegEntry {
	if r.jit == nil {
		return nil
	}
	for k, e := range r.jit.regCache {
		if fn := k.Value(); fn != nil && fn.Name == name && e != nil && e.code != nil {
			return e
		}
	}
	return nil
}

// regMakesLoopsForTest reports whether e's code makes a loop of a
// built-in's call -- a forEach, a map, a filter, ... (ssa.ForEachSite):
// the register form's counterpart of the stack form's entry's loops. Its
// compile's ForEach keeps the built-in it made the loop of among the
// holders its code holds by address (jitRegFeedback's ForEach), which
// nothing else puts there.
func regMakesLoopsForTest(r *Runtime, e *jitRegEntry) bool {
	if e == nil || e.code == nil || e.keep == nil {
		return false
	}
	for _, o := range e.keep.holders {
		if r.jitLoopKind(o) != 0 {
			return true
		}
	}
	return false
}

// regLinkInForTest is how often native callers called e's code through
// its link (jitRegLinks' in), the stack form's e.link.in.
func regLinkInForTest(r *Runtime, e *jitRegEntry) uint64 {
	if e == nil {
		return 0
	}
	if l := r.jit.regLinks[e.key]; l != nil {
		return l.in
	}
	return 0
}

// jitAgreesPairForTest is jitAgreesForTest, which returns the runtime
// with no JIT it compared r's answers with too, for later rounds compared
// the same way (jitAgreeRoundsForTest).
func jitAgreesPairForTest(t *testing.T, setup string, rounds ...string) (r, want *Runtime) {
	t.Helper()
	want = New(Config{})
	t.Cleanup(func() { want.Close(); want.ReleaseClosed() })
	r = jitRuntimeForTest(t, Config{JIT: true})
	for _, rt := range []*Runtime{want, r} {
		if _, err := rt.Run(compileForTest(t, setup)); err != nil {
			t.Fatal(err)
		}
	}
	jitAgreeRoundsForTest(t, r, want, rounds...)
	return r, want
}

// jitAgreeRoundsForTest runs rounds in r and in want, as jitAgreesForTest
// does, and fails at the first answer or error r gives that want does not.
func jitAgreeRoundsForTest(t *testing.T, r, want *Runtime, rounds ...string) {
	t.Helper()
	for i, src := range rounds {
		src = "''+(" + src + ")"
		wv, werr := want.Run(compileForTest(t, src))
		if werr == nil {
			werr = want.runMicrotasks()
		}
		gv, gerr := r.Run(compileForTest(t, src))
		if gerr == nil {
			gerr = r.runMicrotasks()
		}
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("round %d (%s): error %v, interpreter's %v", i, src, gerr, werr)
		}
		if werr != nil {
			if got, want := gerr.Error(), werr.Error(); got != want {
				t.Fatalf("round %d (%s): error %q, interpreter's %q", i, src, got, want)
			}
			continue
		}
		if got, want := gv.String().Go(), wv.String().Go(); got != want {
			t.Fatalf("round %d (%s): got %s, interpreter %s", i, src, got, want)
		}
	}
}

// jitCallbackCode is what the code of a function compiled last does with
// its loops' callbacks, in either form: e is its entry, the stack form's
// or its register code's life (jitRegLifeOf), nil where it has none;
// code says it has native code, loops that it makes a loop of a
// built-in's call; callbacks and unmade how many loops' callbacks it
// inlines and how many of them it does not make (ssa.Func's Callbacks and
// Unmade), inlined how many closures it inlines or its compile asked to
// (the stack form's ssaInlined, the register code's keep's inlined).
type jitCallbackCode struct {
	e                          *jitEntry
	code, loops                bool
	callbacks, unmade, inlined int
}

// jitCallbackCodeForTest is jitCallbackCode of the function named name in
// r, the stack form's entry's (jitCallbackEntry) or its register code's
// (regEntryNamedForTest).
func jitCallbackCodeForTest(r *Runtime, name string) jitCallbackCode {
	if regJITForTest() {
		e := regEntryNamedForTest(r, name)
		if e == nil {
			if l := regLifeNamedForTest(r, name); l != nil {
				return jitCallbackCode{e: l}
			}
			return jitCallbackCode{}
		}
		c := jitCallbackCode{e: e.life, code: true, loops: regMakesLoopsForTest(r, e)}
		if e.keep != nil {
			c.callbacks, c.unmade, c.inlined = e.keep.callbacks, e.keep.unmade, len(e.keep.inlined)
		}
		return c
	}
	e := jitCallbackEntry(r, name)
	if e == nil {
		return jitCallbackCode{}
	}
	return jitCallbackCode{e: e, code: e.ssa != nil, loops: e.loops, callbacks: e.ssaCallbacks, unmade: e.ssaUnmade, inlined: len(e.ssaInlined)}
}

// regLifeNamedForTest is the state that outlives the register code of the
// function named name in r (regLearned), nil where there is none.
func regLifeNamedForTest(r *Runtime, name string) *jitEntry {
	if r.jit == nil {
		return nil
	}
	for k, l := range r.jit.regLearned {
		if fn := k.Value(); fn != nil && fn.Name == name {
			return l
		}
	}
	return nil
}

// jitLinkInNamedForTest is how often native callers called the functions
// named one of names in r through their links, in either form: the stack
// form's entries' links' in, or the register form's regLinks'.
func jitLinkInNamedForTest(r *Runtime, names ...string) uint64 {
	n := uint64(0)
	if regJITForTest() {
		for k, l := range r.jit.regLinks {
			if fn := k.Value(); fn != nil && slices.Contains(names, fn.Name) && l != nil {
				n += l.in
			}
		}
		return n
	}
	for k, x := range r.jit.cache {
		if fn := k.Value(); fn != nil && slices.Contains(names, fn.Name) && x.link != nil {
			n += x.link.in
		}
	}
	return n
}

// jitHintForTest is the entry of cl's function in r, nil where it has
// none: the stack form's (jitState's hint), or, where runtimes run
// register code, the state its register code's compiles share
// (regLearned) -- which counts what the code did, and holds its link.
func jitHintForTest(r *Runtime, cl *closure) *jitEntry {
	if r.jit == nil {
		return nil
	}
	if regJITForTest() {
		return r.jit.regLearned[jitRegKeyInRealmForTest(r, cl)]
	}
	return r.jit.hint(cl)
}

// jitRegKeyInRealmForTest is the key of cl's register code in cl's own
// realm, whichever realm runs now: jitRegKeyOf's in its realm.
func jitRegKeyInRealmForTest(r *Runtime, cl *closure) jitKey {
	if cl.realm == nil || cl.realm == r.Realm {
		return r.jitRegKeyOf(cl)
	}
	re := cl.realm
	if re.jitSelf == (weak.Pointer[Realm]{}) {
		re.jitSelf = weak.Make(re)
	}
	return jitKey{weak.Make(cl.fn), re.jitSelf}
}

// jitRegKeyOfLifeForTest is the key of the register code whose life (the
// state its compiles share) e is.
func jitRegKeyOfLifeForTest(r *Runtime, e *jitEntry) (jitKey, bool) {
	for k, l := range r.jit.regLearned {
		if l == e {
			return k, true
		}
	}
	return jitKey{}, false
}

// jitEntryCodeForTest reports whether e, an entry jitHintForTest gives,
// has code: the stack form's entry's own, or the register code in the
// cache its life (e) belongs to.
func jitEntryCodeForTest(r *Runtime, e *jitEntry) bool {
	if e == nil {
		return false
	}
	if e.jitLearned != nil && e.reg {
		return jitEntryRegCodeForTest(r, e) != nil
	}
	return e.ssa != nil
}

// jitEntryRegCodeForTest is the register code whose life e is, nil where
// the cache holds none with code or e is a stack form's entry.
func jitEntryRegCodeForTest(r *Runtime, e *jitEntry) *jitRegEntry {
	if e == nil || e.jitLearned == nil || !e.reg {
		return nil
	}
	if k, ok := jitRegKeyOfLifeForTest(r, e); ok {
		if c := r.jit.regCache[k]; c != nil && c.code != nil {
			return c
		}
	}
	return nil
}

// jitEntryLinkForTest is the link native callers call e's code through,
// in either form: the stack form's entry's, or the register code's
// (jitLearned's regLink); nil for none.
func jitEntryLinkForTest(e *jitEntry) *jitLink {
	if e == nil {
		return nil
	}
	if e.jitLearned != nil && e.reg {
		return e.regLink
	}
	return e.link
}

// regVirtualsForTest has the register compiles made until stop is called
// note, by their function, how many objects the code compiled last does
// not make (ssa.Func's Virtuals): the register form's counterpart of the
// stack form's entry's ssaVirtuals, which its entries do not keep; each
// runtime compiles functions of its own. It sets jitSSAOptimized, which
// every compile reads, and which keeps compiles from the background: a
// test that calls it does not run in parallel with others, nor ask for
// compiles in the background meanwhile.
func regVirtualsForTest(t *testing.T) (virtuals func(fn *bytecode.Function) int, stop func()) {
	var mu sync.Mutex
	noted := map[*bytecode.Function]int{}
	jitSSAOptimized = func(fn *bytecode.Function, f *ssa.Func) {
		mu.Lock()
		noted[fn] = f.Virtuals
		mu.Unlock()
	}
	stop = func() { jitSSAOptimized = nil }
	t.Cleanup(stop)
	return func(fn *bytecode.Function) int {
		mu.Lock()
		defer mu.Unlock()
		return noted[fn]
	}, stop
}

// jitVirtualsForTest is, for what runs until stop is called, how many
// objects the code compiled last of the function named name in a runtime
// does not make, and whether it has code, in either form: the stack
// form's entry's ssaVirtuals, or what the register compiles noted
// (regVirtualsForTest), which a test that asks does not run in parallel
// where runtimes run register code.
func jitVirtualsForTest(t *testing.T) (virtuals func(r *Runtime, name string) (code bool, n int), stop func()) {
	if regJITForTest() {
		noted, stop := regVirtualsForTest(t)
		return func(r *Runtime, name string) (bool, int) {
			for k, e := range r.jit.regCache {
				if fn := k.Value(); fn != nil && fn.Name == name && e != nil && e.code != nil {
					return true, noted(fn)
				}
			}
			return false, 0
		}, stop
	}
	return func(r *Runtime, name string) (bool, int) {
		e := jitCallbackEntry(r, name)
		if e == nil || e.ssa == nil {
			return false, 0
		}
		return true, e.ssaVirtuals
	}, func() {}
}

// regRebuiltVirtualsForTest is how many objects the register code of the
// global function name in r leaves unmade, and whether it has code: the
// program the code was compiled from built and optimized again with the
// function's feedback as it is now, as JITCompileRepeat's compiles build
// it again (jitRegCompileRepeat) -- for code compiled in the background,
// whose build no compile's hook sees (regVirtualsForTest).
func regRebuiltVirtualsForTest(t *testing.T, r *Runtime, name string) (bool, int) {
	t.Helper()
	cl := r.global.getOwn(r.atoms.intern(name)).value().Object().fn().closure
	e := r.jit.regCache[r.jitRegKeyOf(cl)]
	if e == nil || e.code == nil || e.p == nil {
		return false, 0
	}
	w := jit.GetWorkspace()
	defer jit.PutWorkspace(w)
	f, err := ssa.BuildIn(&w.SSA, e.p, r.jitRegFeedbackFor(cl, e.p))
	if err != nil {
		t.Fatalf("%s built again: %v", name, err)
	}
	jitRegShape(f, e.p, cl, cl.reg)
	ssa.Optimize(f)
	return true, f.Virtuals
}

// jitEntryNamedForTest is the entry of the function named name in r, in
// either form: the stack form's (jitCallbackEntry), or the state its
// register code's compiles share (regLifeNamedForTest), whose targets
// (jitTargets) and counts are the stack form's entry's counterparts.
func jitEntryNamedForTest(r *Runtime, name string) *jitEntry {
	if regJITForTest() {
		return regLifeNamedForTest(r, name)
	}
	return jitCallbackEntry(r, name)
}

// jitCachedForTest is what r's JIT's cache holds for the global function
// name, in either form, nil for nothing: the stack form's entry, or, where
// runtimes run register code, its register code's entry (regCache) --
// compared by identity, as a test asks whether the cache still holds the
// same; code says whether it holds code.
func jitCachedForTest(r *Runtime, name string) (held any, code bool) {
	cl := jitGlobalClosure(r, name)
	if regJITForTest() {
		if e := r.jit.regCache[r.jitRegKeyOf(cl)]; e != nil {
			return e, e.code != nil
		}
		return nil, false
	}
	if e := r.jit.cache[r.jitKeyOf(cl.fn)]; e != nil {
		return e, e.ssa != nil
	}
	return nil, false
}

// regLinksHeldForTest is jitLinksHeld for register code: every link native
// callers call through calls into the code the cache holds for its
// function (regLinks, regCache).
func regLinksHeldForTest(s *jitState) error {
	for key, l := range s.regLinks {
		if l.entry == 0 {
			continue
		}
		switch e := s.regCache[key]; {
		case e == nil || e.code == nil:
			return fmt.Errorf("a link calls %#x, of code the cache does not hold", l.entry)
		case !e.code.Contains(l.entry):
			return fmt.Errorf("a link calls %#x, outside its function's code", l.entry)
		}
	}
	return nil
}

// regEntriesFoundForTest is jitEntriesFound for register code: every
// entry the cache holds is marked held (jitRegEntry's cached), not let go
// of, and is the one its key names.
func regEntriesFoundForTest(s *jitState) error {
	for key, e := range s.regCache {
		if !e.cached || e.dropped || e.key != key {
			return fmt.Errorf("the cache holds an entry let go of, or not its key's: cached %v, dropped %v", e.cached, e.dropped)
		}
	}
	return nil
}

// jitCalleeCodeForTest is the entry of the global function name, the link
// native callers call its code through and its code, in either form: the
// stack form's entry (jitNativeOrCalleeForTest), or its register code's
// life, link and code. It fails the test where the function has no code,
// or no link.
func jitCalleeCodeForTest(t *testing.T, r *Runtime, name string) (*jitEntry, *jitLink, *jit.SSACode) {
	t.Helper()
	if regJITForTest() {
		cl := jitGlobalClosure(r, name)
		key := r.jitRegKeyOf(cl)
		e := r.jit.regCache[key]
		if e == nil || e.code == nil || e.life == nil {
			t.Fatalf("%s has no code: %+v", name, e)
		}
		l := r.jit.regLinks[key]
		if l == nil {
			t.Fatalf("%s has no link: %+v", name, e)
		}
		return e.life, l, e.code
	}
	e := jitNativeOrCalleeForTest(t, r, name)
	if e.link == nil {
		t.Fatalf("%s has no link: %+v", name, e)
	}
	return e, e.link, e.ssa
}
