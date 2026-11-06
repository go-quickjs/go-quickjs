package vm

import (
	"fmt"
	"runtime"
	"testing"
	"weak"

	"github.com/go-quickjs/go-quickjs/internal/bytecode"
)

func TestArrayLiteralRoom(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function fill(n) {
		var a = [];
		for (var i = 0; i < n; i++) a.push(i);
		return a;
	}`)); err != nil {
		t.Fatal(err)
	}
	for round, n := range []int{30000, 30000, 30000, 30000, 0, 0, 30000, 0, 4, 70000, 70000, 70000} {
		v, err := r.Run(compileForTest(t, fmt.Sprintf("fill(%d)", n)))
		if err != nil {
			t.Fatal(err)
		}
		els := v.Object().elemsOf()
		if len(els) != n {
			t.Fatalf("length %d, want %d", len(els), n)
		}
		if round == 3 && cap(els) != n {
			t.Fatalf("fourth array has capacity %d, want its learned length %d", cap(els), n)
		}
		if round == 5 && cap(els) != len(arrayObject{}.inline) {
			t.Fatalf("short array kept capacity %d", cap(els))
		}
		for i, v := range els {
			if !v.IsNumber() || v.Number() != float64(i) {
				t.Fatalf("element %d is %v", i, v)
			}
		}
		if len(r.arraySamples) != 0 {
			t.Fatal("turn kept array samples")
		}
		fn := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
		if len(fn.arrays) != 1 {
			t.Fatalf("%d literal sites, want 1", len(fn.arrays))
		}
		s := fn.arrays[0]
		if s.sample != nil || s.weak.Value() != nil || s.listed {
			t.Fatal("literal kept its sample after the turn")
		}
		if s.room > arrayRoomLimit || (n < 256 || n > arrayRoomLimit) && s.room != 0 {
			t.Fatalf("length %d learned room %d", n, s.room)
		}
	}
}

func TestArrayLiteralRoomFirstUse(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{})
	defer r.Close()
	r.SetEvaluator(func(source string, _ EvalRequest) (*bytecode.Function, error) {
		return compileForTest(t, source), nil
	})
	checks := 0
	r.global.setOwnRaw(r.atoms.intern("checkSamples"), r.NewFunction("checkSamples", 1,
		func(rt *Runtime, _ Value, args []Value) (Value, error) {
			checks++
			if len(rt.arraySamples) != int(args[0].Number()) {
				t.Fatalf("check %d: %d pending samples, want %v", checks, len(rt.arraySamples), args[0])
			}
			return Undefined, nil
		}), propWritable|propConfigurable)
	if _, err := r.Run(compileForTest(t, `
		for (var i = 0; i < 20; i++) {
			(0, eval)('(function(){var a=[];a.push(1);return a})()');
		}
		checkSamples(0);
		function fill() { var a = []; a.push(1); return a; }
		var a = fill();
		checkSamples(0);
		a = fill();
		checkSamples(1);
	`)); err != nil {
		t.Fatal(err)
	}
	if checks != 3 {
		t.Fatalf("%d checks, want 3", checks)
	}
}

func TestArrayLiteralRoomSamples(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	a := newArrayObject(r.proto.array, 30000)
	s := &arrayRoom{sample: a, room: 30000, previous: 30000, listed: true}
	r.arraySamples = []*arrayRoom{s}
	r.frameDepth = 1
	arraySamplesLetGo[struct{}](r)
	if s.sample != nil || s.weak.Value() != a || !s.listed {
		t.Fatal("GC sweep did not weaken its sample")
	}
	// The same live array can grow past the sweep. Its final length is
	// what the next turn records, without keeping its values afterwards.
	a.extOf().elems = append(a.elemsOf(), make([]Value, 10000)...)
	r.frameDepth = 0
	arraySamplesLetGo[struct{}](r)
	if s.previous != 40000 || s.room != 30000 || s.weak.Value() != nil || len(r.arraySamples) != 0 {
		t.Fatalf("final sample: room %d, previous %d, pending %d", s.room, s.previous, len(r.arraySamples))
	}
	// A shorter sample gives back room even if the next GC collects it
	// before a later literal can observe it.
	a.extOf().elems = a.elemsOf()[:0]
	s.sample, s.listed = a, true
	r.arraySamples = append(r.arraySamples, s)
	r.frameDepth = 1
	arraySamplesLetGo[struct{}](r)
	if s.room != 0 {
		t.Fatalf("empty sample kept room %d", s.room)
	}
	r.frameDepth = 0
	arraySamplesLetGo[struct{}](r)
}

func TestArrayLiteralRoomCollected(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	var w weak.Pointer[Object]
	func() {
		a := newArrayObject(r.proto.array, 1000)
		w = weak.Make(a)
		s := &arrayRoom{sample: a, listed: true}
		r.arraySamples = []*arrayRoom{s}
		r.frameDepth = 1
		arraySamplesLetGo[struct{}](r)
	}()
	for i := 0; i < 10 && w.Value() != nil; i++ {
		runtime.GC()
	}
	if w.Value() != nil {
		t.Fatal("GC sample kept its array alive")
	}
	arraySamplesLetGo[struct{}](r)
	r.frameDepth = 0
	if len(r.arraySamples) != 0 {
		t.Fatal("dead sample stayed pending")
	}
}

func TestArrayLiteralRoomRealms(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{})
	defer r.Close()
	fn := compileForTest(t, `function fill(n) {
		var a = [], b = [];
		for (var i = 0; i < n; i++) a.push(i);
		b.push(n);
		return [a, b];
	}`)
	var sites []*arrayRoom
	for _, n := range []int{512, 1024, 2048} {
		re := r.NewRealm()
		if _, err := r.RunIn(re, fn); err != nil {
			t.Fatal(err)
		}
		call := compileForTest(t, fmt.Sprintf("fill(%d)", n))
		for range 3 {
			v, err := r.RunIn(re, call)
			if err != nil {
				t.Fatal(err)
			}
			pair := v.Object().elemsOf()
			if len(pair[0].Object().elemsOf()) != n || pair[1].Object().elemsOf()[0].Number() != float64(n) {
				t.Fatal("array literal changed its contents")
			}
			if pair[0].Object().proto() != re.proto.array || pair[1].Object().proto() != re.proto.array {
				t.Fatal("array literal has another realm's prototype")
			}
		}
		cl := re.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
		if len(cl.arrays) != 2 || cl.arrays[0].room != n || cl.arrays[1].room != 0 {
			t.Fatalf("realm's two literal sites did not learn separately: %v", cl.arrays)
		}
		sites = append(sites, cl.arrays[0])
	}
	for i, s := range sites {
		if s.room != 512<<i || s.sample != nil || s.weak.Value() != nil {
			t.Fatalf("realm %d has another realm's feedback or kept its sample", i)
		}
	}
}

func TestArrayLiteralRoomMemoryLimit(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{MemoryLimit: 64 << 20})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function fill() {
		var a = [];
		for (var i = 0; i < 1024; i++) a.push(i);
		return a;
	}`)); err != nil {
		t.Fatal(err)
	}
	call := compileForTest(t, "fill()")
	for range 4 {
		v, err := r.Run(call)
		if err != nil || len(v.Object().elemsOf()) != 1024 {
			t.Fatalf("metered array: %v, %v", v, err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
	if len(cl.arrays) != 0 || len(r.arraySamples) != 0 {
		t.Fatal("memory-limited runtime learned extra array room")
	}
}

func TestArrayLiteralRoomAfterSmall(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function fill(n) {
		var a = [];
		for (var i = 0; i < n; i++) a.push(i);
		return a;
	}`)); err != nil {
		t.Fatal(err)
	}
	short := compileForTest(t, "fill(4)")
	long := compileForTest(t, "fill(4096)")
	for range 64 {
		if _, err := r.Run(short); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
	for range 35 {
		if _, err := r.Run(long); err != nil {
			t.Fatal(err)
		}
	}
	if cl.arrays[0].room != 4096 {
		t.Fatal("site did not resume sampling after its arrays grew longer")
	}
	for i := range 1024 {
		call := short
		if i&31 == 0 {
			call = long
		}
		if _, err := r.Run(call); err != nil {
			t.Fatal(err)
		}
		if i > 2 && i&31 != 0 && cl.arrays[0].room != 0 {
			t.Fatalf("short result %d learned room from nonconsecutive long samples", i)
		}
	}
}

// The learned room changes where an append reallocates. Callback methods
// must read subsequent mutations from the array's current elements.
func TestArrayLiteralRoomCallbackGrowth(t *testing.T) {
	was := treeTier.Load()
	SetTreeTier(true)
	t.Cleanup(func() { SetTreeTier(was) })
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function fill(n) {
		var a = [];
		for (var i = 0; i < n; i++) a.push(i);
		return a;
	}`)); err != nil {
		t.Fatal(err)
	}
	call := compileForTest(t, "fill(4096)")
	for range 3 {
		if _, err := r.Run(call); err != nil {
			t.Fatal(err)
		}
	}
	cl := r.global.getOwn(r.atoms.intern("fill")).value().Object().fn().closure
	if len(cl.arrays) != 1 || cl.arrays[0].room != 4096 {
		t.Fatal("callback test did not learn its array's exact room")
	}
	v, err := r.Run(compileForTest(t, `function change(a) {
		a.push(4096, 4097); a.pop(); a[100] = 9000; delete a[200]; a[300] = -1;
	}
	var a = fill(4096);
	var mapped = a.map(function(v, i) { if (i === 0) change(a); return v; });
	var out = [a.length, mapped.length, mapped[100], 200 in mapped, mapped[300], mapped[4095]];
	a = fill(4096);
	out.push(a.reduce(function(s, v, i) { if (i === 0) change(a); return s + v; }, 0));
	a = fill(4096);
	out.push(a.filter(function(v, i) { if (i === 0) change(a); return v >= 4095; }).join(','));
	a = fill(4096);
	var count = 0, sum = 0;
	a.forEach(function(v, i) { if (i === 0) change(a); count++; sum += v; });
	out.push(count, sum);
	a = fill(4096);
	out.push(a.some(function(v, i) { if (i === 0) change(a); return i === 100 && v === 9000; }));
	a = fill(4096); count = 0;
	out.push(a.every(function(v, i) { if (i === 0) change(a); count++; return i !== 100 || v === 9000; }), count);
	out.join('|')`))
	const want = "4097|4096|9000|false|-1|4095|8394959|9000,4095|4095|8394959|true|true|4095"
	if err != nil || !v.IsString() || v.String().Go() != want {
		t.Fatalf("callback growth: %v, %v; want %s", v, err, want)
	}
}
