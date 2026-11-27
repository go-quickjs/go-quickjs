package decoder

import (
	"bytes"
	"runtime"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/internal/module"
)

// What validating a module costs grows with its size, not with the square
// of how deep its blocks nest or its supertype chains reach.

// nestedBody is a function body of n constructs, each opened by open and
// holding code, then closed.
func nestedBody(n int, open, code []byte) []byte {
	var b []byte
	b = append(b, 0) // no locals
	for i := 0; i < n; i++ {
		b = append(b, open...)
		b = append(b, code...)
	}
	b = append(b, bytes.Repeat([]byte{0x0b}, n+1)...)
	return cat(uleb(uint64(len(b))), b)
}

// validateModule decodes and validates b under l.
func validateModule(t *testing.T, b []byte, l *Limits) {
	t.Helper()
	m, err := Decode(b, l)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFunctions(m, l); err != nil {
		t.Fatal(err)
	}
}

// validateAllocs returns the bytes validating b under l allocates.
func validateAllocs(t *testing.T, b []byte, l *Limits) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	validateModule(t, b, l)
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestBrTableAllocatesByTargets checks that a br_table allocates for its
// targets, not for every construct open around it: a body of nested
// blocks each holding a br_table allocates in proportion to its size.
func TestBrTableAllocatesByTargets(t *testing.T) {
	// block; i32.const 0; br_table [] 0
	open := bs(0x02, tVoid)
	code := bs(0x41, 0, 0x0e, 0, 0)
	const n = 8000
	b := modSpec{types: [][]byte{voidFn}, bodies: [][]byte{nestedBody(n, open, code)}}.bytes()
	l := CoreLimits()
	validateAllocs(t, b, l) // the validators' stacks, made once
	got := validateAllocs(t, b, l)
	t.Logf("validating %d nested br_tables (%d bytes) allocated %d bytes", n, len(b), got)
	if limit := uint64(64 * len(b)); got > limit {
		t.Errorf("validating %d nested br_tables (%d bytes) allocated %d bytes, more than %d", n, len(b), got, limit)
	}
}

// runTime returns the time f takes a run, repeated until it takes long
// enough to time.
func runTime(f func()) time.Duration {
	reps := 0
	start := time.Now()
	for {
		f()
		reps++
		if d := time.Since(start); d >= 20*time.Millisecond {
			return d / time.Duration(reps)
		}
	}
}

// checkLinear times validating build(n) and build(8n) and fails if the
// larger takes more than four times the eightfold a linear cost would.
// The two are timed in turn, the least time of each kept, so that what
// else the machine runs meets both alike: timed one after the other, a
// busy spell during the larger's runs alone had made it 40 times the
// smaller on a shared runner, where it is 9 times on an idle one.
func checkLinear(t *testing.T, n int, build func(n int) []byte) {
	t.Helper()
	l := CoreLimits()
	small, large := build(n), build(8*n)
	validateModule(t, small, l)
	validateModule(t, large, l)
	ts, tl := time.Duration(1<<62), time.Duration(1<<62)
	for range 5 {
		ts = min(ts, runTime(func() { validateModule(t, small, l) }))
		tl = min(tl, runTime(func() { validateModule(t, large, l) }))
	}
	ratio := float64(tl) / float64(ts)
	t.Logf("%d bytes: %v; %d bytes: %v; ratio %.1f", len(small), ts, len(large), tl, ratio)
	if ratio > 32 {
		t.Errorf("validating 8 times the module took %.1f times as long (%v, %v); want about 8", ratio, ts, tl)
	}
}

// TestNestedTryLinear checks that validating nested legacy trys takes time
// in proportion to their number.
func TestNestedTryLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("times validation")
	}
	checkLinear(t, 8000, func(n int) []byte {
		body := nestedBody(n, bs(0x06, tVoid), nil)
		return modSpec{types: [][]byte{voidFn}, bodies: [][]byte{body}}.bytes()
	})
}

// subtypeChainTypes is a type section of a chain of n struct types, each
// the subtype of the one before, and of n struct subtypes of one more, each
// with a field naming a type of the chain where its supertype's names the
// chain's first: each says it is a subtype by a check that compares a type
// of the chain with the first.
func subtypeChainTypes(n int) [][]byte {
	types := [][]byte{bs(0x50, 0, 0x5f, 0)} // 0: (sub (struct))
	for i := 1; i < n; i++ {
		types = append(types, cat(0x50, 1, uleb(uint64(i-1)), 0x5f, 0)) // i: (sub i-1 (struct))
	}
	base := uint64(n)
	types = append(types, cat(0x50, 0, 0x5f, 1, tRef, sleb(0), 0)) // n: (sub (struct (field (ref 0))))
	for i := 0; i < n; i++ {
		// (sub n (struct (field (ref i))))
		types = append(types, cat(0x50, 1, uleb(base), 0x5f, 1, tRef, sleb(int64(i)), 0))
	}
	return types
}

// TestSubtypeChainLinear checks that a subtype check over a long supertype
// chain does not take a step a link: validating a type section that makes
// a check of each length up to n takes time in proportion to its size.
func TestSubtypeChainLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("times validation")
	}
	checkLinear(t, 4000, func(n int) []byte {
		return modSpec{types: subtypeChainTypes(n)}.bytes()
	})
}

// TestSubtypeChainAnswers checks the subtype checks of a long chain: a
// type is a subtype of each of its chain's types and of no type after it,
// and of a type of the same canonical index in another chain.
func TestSubtypeChainAnswers(t *testing.T) {
	const n = 300
	types := subtypeChainTypes(n)
	// A second chain, the same types as the first: n+1+n ... 3n.
	second := len(types)
	types = append(types, bs(0x50, 0, 0x5f, 0))
	for i := 1; i < n; i++ {
		types = append(types, cat(0x50, 1, uleb(uint64(second+i-1)), 0x5f, 0))
	}
	m, err := Decode(modSpec{types: types}.bytes(), CoreLimits())
	if err != nil {
		t.Fatal(err)
	}
	ref := func(i int) module.ValueType { return module.Ref(module.HeapIndex(uint32(i))) }
	for _, c := range []struct {
		sub, super int
		want       bool
	}{
		{n - 1, 0, true}, {n - 1, n / 2, true}, {n / 2, n - 1, false}, {0, 0, true},
		{n - 1, second, true}, {second + n - 1, 0, true}, {second + n - 1, n - 2, true},
		{second + 1, n - 1, false}, {n - 1, n, false}, {n + 5, n, true}, {n, n + 5, false},
	} {
		if got := module.IsSubtypeOf(ref(c.sub), ref(c.super), m); got != c.want {
			t.Errorf("(ref %d) <: (ref %d) = %v, want %v", c.sub, c.super, got, c.want)
		}
	}
	// A struct of the chain declaring a supertype past it is not valid.
	bad := subtypeChainTypes(n)
	bad = append(bad, cat(0x50, 1, uleb(uint64(n+1)), 0x5f, 1, tRef, sleb(int64(n+1)), 0))
	if _, err := Decode(modSpec{types: bad}.bytes(), CoreLimits()); err == nil {
		t.Error("a field naming its supertype's field's type's supertype decoded")
	}
}

// TestSubtypeTreeAnswers compares the subtype checks over a tree of struct
// types, a second copy of it and the canonical types with a walk of the
// chains of supertypes a link at a time.
func TestSubtypeTreeAnswers(t *testing.T) {
	const n = 160
	parent := make([]int, n)
	types := [][]byte{bs(0x50, 0, 0x5f, 0)}
	parent[0] = -1
	for i := 1; i < n; i++ {
		// Long chains and branches: mostly the type before, at times one
		// further up.
		p := i - 1
		if i%7 == 0 {
			p = i / 3
		}
		parent[i] = p
		types = append(types, cat(0x50, 1, uleb(uint64(p)), 0x5f, 0))
	}
	for i := 0; i < n; i++ {
		if parent[i] < 0 {
			types = append(types, bs(0x50, 0, 0x5f, 0))
			continue
		}
		types = append(types, cat(0x50, 1, uleb(uint64(n+parent[i])), 0x5f, 0))
	}
	m, err := Decode(modSpec{types: types}.bytes(), CoreLimits())
	if err != nil {
		t.Fatal(err)
	}
	walk := func(sub, super int) bool {
		for i := sub; ; i = int(m.Types[i].Super) {
			if m.Canonical[i] == m.Canonical[super] {
				return true
			}
			if m.Types[i].Super == module.NoSuper {
				return false
			}
		}
	}
	for i := range 2 * n {
		depth := 0
		for j := i; parent[j%n] >= 0; j = parent[j%n] {
			depth++
		}
		if m.Types[i].Depth != uint32(depth) || module.Canonical(m.Canonical[i]).Depth != uint32(depth) {
			t.Fatalf("type %d: depth %d, canonical depth %d, want %d", i, m.Types[i].Depth, module.Canonical(m.Canonical[i]).Depth, depth)
		}
	}
	for sub := range 2 * n {
		for super := range 2 * n {
			want := walk(sub, super)
			if got := module.IsHeapSubtypeOf(module.HeapIndex(uint32(sub)), module.HeapIndex(uint32(super)), m); got != want {
				t.Fatalf("%d <: %d = %v, want %v", sub, super, got, want)
			}
			if got := module.IsCanonicalSubtype(m.Canonical[sub], m.Canonical[super]); got != want {
				t.Fatalf("canonical %d <: %d = %v, want %v", sub, super, got, want)
			}
			// The least common supertype is the first of sub's chain above
			// super.
			var common module.HeapType
			for i := sub; ; i = int(m.Types[i].Super) {
				if walk(super, i) {
					common = module.HeapIndex(uint32(i))
					break
				}
			}
			if walk(sub, super) {
				common = module.HeapIndex(uint32(super))
			} else if walk(super, sub) {
				common = module.HeapIndex(uint32(sub))
			}
			u, ok := module.Union(module.Ref(module.HeapIndex(uint32(sub))), module.Ref(module.HeapIndex(uint32(super))), m)
			if !ok || u.Heap != common {
				t.Fatalf("union of %d and %d = %v %v, want %v", sub, super, u.Heap, ok, common)
			}
		}
	}
}
