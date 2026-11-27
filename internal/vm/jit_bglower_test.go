//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

// A compile in the background keeps the programs each kind's lowering
// makes: each kept in the runtime's lowerings is what jitRegLowerWith makes
// of its register code and hints, the kinds that share their program
// lowered once (jitRegLowerKinds), and the code gives the interpreter's
// answers. The compiles are handed over with no program, their own
// function lowered by their worker (lowerRoot).
func TestJITBackgroundSamePrograms(t *testing.T) {
	t.Parallel()
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			prog := compileForTest(t, regBackgroundSrc)
			want := ""
			{
				r := newTestRuntime(t, Config{})
				t.Cleanup(r.Close)
				if _, err := r.Run(prog); err != nil {
					t.Fatal(err)
				}
				fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
				for range 6 {
					v, err := r.Call(fn, Undefined, []Value{Float(2000)})
					want += regNativeShow(r, v, err) + ";"
				}
			}
			var mu sync.Mutex
			roots := 0
			jitTestSwitch(t, func(c *jitTestConfig) {
				c.bgReplayed = func(j *jitRegJob) {
					mu.Lock()
					defer mu.Unlock()
					if j.root != nil && j.root.lowered && j.p != nil {
						roots++
					}
				}
			})
			r := newTestRuntime(t, Config{JIT: true})
			t.Cleanup(r.Close)
			r.SetJITWarmUp(1, 1)
			r.SetJITBackground(true)
			r.jitStress.background, r.jitStress.checks = true, 1
			if _, err := r.Run(prog); err != nil {
				t.Fatal(err)
			}
			fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
			got := ""
			for range 6 {
				v, err := r.Call(fn, Undefined, []Value{Float(2000)})
				got += regNativeShow(r, v, err) + ";"
			}
			if got != want {
				t.Fatalf("got %q, want the interpreter's %q", got, want)
			}
			s := r.jit
			if s.bgSubmitted == 0 {
				t.Fatal("no compile handed over")
			}
			mu.Lock()
			if roots == 0 {
				t.Error("no compile's function lowered on its worker")
			}
			mu.Unlock()
			if s.regSnapMisses != 0 {
				t.Errorf("%d snapshot misses", s.regSnapMisses)
			}
			for key, l := range s.regLowered {
				fn := key.Value()
				if fn == nil {
					continue
				}
				for kind, p := range l.progs {
					if !l.done[kind] {
						continue
					}
					h := l.hints
					h.KeepParams = r.nodeQuirks && !fn.Strict
					q, err := jitRegLowerWith(fn.RegCompiled(), fn, jitLowerKind(kind), h)
					if !reflect.DeepEqual(p, q) || fmt.Sprint(err) != fmt.Sprint(l.errs[kind]) {
						t.Errorf("%s's program of kind %d (error %v) is not the goroutine's lowering (error %v)", fn.Name, kind, l.errs[kind], err)
					}
				}
			}
		})
	}
}

// regReplaySrc has work call big natively, through a link (too long to
// inline), and norm inlined.
const regReplaySrc = `
function Point(x, y) { this.x = x; this.y = y }
function norm(p) { return p.x * p.x + p.y * p.y }
function big(a) {
	let s = a;
	for (let i = 0; i < 3; i++) { s = (s * 31 + i) % 1000003; s = (s ^ (s >> 3)) % 1000003; s = (s + a * i) % 1000003;
		s = (s * 7 + 1) % 1000003; s = (s ^ (s << 2)) % 1000003; s = (s + 11) % 1000003; s = (s * 13) % 1000003 }
	return s
}
function work(n) {
	let s = 0;
	for (let i = 0; i < n; i++) {
		const p = new Point(i, i + 1);
		s = (s + norm(p) + big(i)) % 1000003;
	}
	return s;
}
`

// A compile's worker puts back what its snapshot's questions took back
// (workRecovering's replay), and the runtime's goroutine, which may unlink
// a link meanwhile (regUnlink), finds every link the replay puts in the
// compile's keep among those it reads (holdsLink): those the keep held at
// the hand-over and those the questions took back -- links are made only
// on the goroutine, never by a worker.
func TestJITReplayLinksHeldAtHandOver(t *testing.T) {
	t.Parallel()
	prog := compileForTest(t, regReplaySrc)
	want := ""
	{
		r := newTestRuntime(t, Config{})
		t.Cleanup(r.Close)
		if _, err := r.Run(prog); err != nil {
			t.Fatal(err)
		}
		fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
		for range 6 {
			v, err := r.Call(fn, Undefined, []Value{Float(500)})
			want += regNativeShow(r, v, err) + ";"
		}
	}
	var mu sync.Mutex
	replays, links := 0, 0
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.bgReplayed = func(j *jitRegJob) {
			mu.Lock()
			defer mu.Unlock()
			replays++
			for _, l := range j.rf.fb.keep().links {
				links++
				if !slices.Contains(j.links, l) && !slices.Contains(j.snap.s.gained.links, l) {
					t.Errorf("%s: the replay put a link in its keep that the goroutine does not see", j.cl.fn.Name)
				}
			}
		}
	})
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	r.SetJITWarmUp(1, 1)
	r.SetJITBackground(true)
	r.jitStress.background, r.jitStress.checks = true, 1
	if _, err := r.Run(prog); err != nil {
		t.Fatal(err)
	}
	fn, _ := r.GetProp(Obj(r.Global()), r.Intern("work"))
	got := ""
	for range 6 {
		v, err := r.Call(fn, Undefined, []Value{Float(500)})
		got += regNativeShow(r, v, err) + ";"
	}
	if got != want {
		t.Fatalf("got %q, want the interpreter's %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if replays == 0 || links == 0 {
		t.Fatalf("%d replays, %d links in their keeps: want some of each", replays, links)
	}
}

// A lowering made on a worker is kept at its install (regKeepLoweredAt)
// as one made on the runtime's goroutine is: its programs for the kinds
// the function's lowering has none of, counted once each in what the JIT
// holds; nothing kept a second time, nor where the function's hints have
// grown since it was made. The hints it was made with are taken at the
// gather by jitRegHintsCopy, which writes nothing: no state made for the
// function, its lowerings untouched, and once kept, the programs are what
// it answers with.
func TestJITKeepLoweredAt(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{JIT: true})
	t.Cleanup(r.Close)
	// Called once, made its register code, and compiled never.
	r.SetJITWarmUp(60000, 60000)
	if _, err := r.Run(compileForTest(t, `function loop(n){let s=0;for(let i=0;i<n;i++)s=(s+i)|0;return s}; loop(1)`)); err != nil {
		t.Fatal(err)
	}
	cl := jitGlobalClosureForTest(r, "loop")
	rc := cl.reg.rc
	key := r.jitRegKeyOf(cl)
	s := r.jitState()
	learned, lowered, held := s.regLearned[key], s.regLowered[key], s.lowerBytes
	if lowered != nil && lowered.done[jitLowerSSA] {
		t.Fatal("loop lowered before the test")
	}
	// A mark the interpreter has made, not taken yet.
	add := -1
	for pc, w := range rc.Code {
		switch w.Op() {
		case bytecode.RAdd, bytecode.RAddI, bytecode.RAddI32, bytecode.RAddII32:
			add = pc
		}
	}
	if add < 0 {
		t.Fatalf("no addition in loop's register code:\n%s", rc)
	}
	regStringAddOf(cl.reg, add)
	k, h, cached := r.jitRegHintsCopy(cl, rc)
	if k != key || cached != nil || !slices.Contains(h.StringAdds, int32(add)) {
		t.Fatalf("key %v, cached %v, hints %+v: want loop's, none cached, the mark taken into the copy", k, cached, h)
	}
	if s.regLearned[key] != learned || s.regLowered[key] != lowered || s.lowerBytes != held ||
		learned != nil && slices.Contains(learned.regHints.StringAdds, int32(add)) {
		t.Fatal("the copy wrote the runtime's state")
	}
	got := jitRegLowerKinds(rc, cl.fn, jitLowerSSA, h)
	if !r.regKeepLoweredAt(cl, rc, &h, &got) {
		t.Fatal("not kept")
	}
	l := s.regLowered[key]
	if l == nil || l.progs[jitLowerSSA] != got.progs[jitLowerSSA] || l.progs[jitLowerInline] != got.progs[jitLowerSSA] {
		t.Fatalf("not kept: %+v", l)
	}
	n := got.progs[jitLowerSSA].Footprint()
	if lowered == nil {
		// And the lowering's own, made for it.
		n += jitLoweringFixed
	}
	if s.lowerBytes-held != n {
		t.Fatalf("counted %d, want the program's once, %d", s.lowerBytes-held, n)
	}
	if r.regKeepLoweredAt(cl, rc, &h, &got) || s.lowerBytes-held != n {
		t.Fatal("kept again")
	}
	if _, h2, cached := r.jitRegHintsCopy(cl, rc); cached == nil || cached.progs[jitLowerSSA] != got.progs[jitLowerSSA] || !jitHintsSame(&h2, &h) {
		t.Fatal("the kept programs are not the copy's answer")
	}
	// Hints grown since: nothing kept.
	s.relower(l)
	s.regLearnedOf(key).regHints.WholeOps = append(s.regLearnedOf(key).regHints.WholeOps, 1)
	if r.regKeepLoweredAt(cl, rc, &h, &got) {
		t.Fatal("kept, made with hints the function has outgrown")
	}
}

// A keep's links held as its code is placed are each once, in the order of
// their first (jitLinksOnce), however many and however often repeated --
// by a scan for a few, a set for more -- and a hold counts each once: up
// as the code is placed, down as it is closed, the links let go of by
// none meanwhile.
func TestJITLinksOnce(t *testing.T) {
	t.Parallel()
	for _, n := range []int{3, 16, 17, 200} {
		links := make([]*jitLink, n)
		for i := range links {
			links[i] = &jitLink{}
		}
		var keep []*jitLink
		for i := range 4 * n {
			// Each repeated, the first of each in order: 0, 1, 0, 2, 1, 3, ...
			keep = append(keep, links[(i/2+i%2*(i/4))%n])
		}
		var want []*jitLink
		for _, l := range keep {
			if !slices.Contains(want, l) {
				want = append(want, l)
			}
		}
		fb := &jitFeedback{links: slices.Clone(keep)}
		s := &jitState{}
		s.regHoldLinks(fb, 1)
		if !slices.Equal(fb.links, want) || !fb.linksOnce {
			t.Fatalf("%d links: held %d, want each of %d once in the order of its first", n, len(fb.links), len(want))
		}
		for i, l := range want {
			if l.refs != 1 {
				t.Fatalf("%d links: link %d held %d times", n, i, l.refs)
			}
		}
		// Held again by its other code, then let go of by one: each held once
		// still.
		s.regHoldLinks(fb, 1)
		s.regHoldLinks(fb, -1)
		for i, l := range want {
			if l.refs != 1 {
				t.Fatalf("%d links: link %d held %d times after a hold and a let-go", n, i, l.refs)
			}
		}
	}
}
