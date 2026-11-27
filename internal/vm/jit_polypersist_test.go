//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"bytes"
	"slices"
	"testing"
	"unsafe"
)

// jitPolyPersistSrc has its property sites' exits learn every kind of what
// a site keeps (jitPolySite): the shapes a read met past its cache (cases),
// past more than the cases hold (missed, mega), a getter on the receivers'
// prototypes (accessors), a read past two prototypes (reads), an addition
// along a chain deeper than a cache holds (adds), a primitive's read (prim).
const jitPolyPersistSrc = `
function read(o) { return o.k }
function many(n, w) {
	let s = 0
	for (let i = 0; i < n; i++) {
		const o = i % w === 0 ? {k: 1} : i % w === 1 ? {a: 0, k: 2} : i % w === 2 ? {b: 0, k: 3} : i % w === 3 ? {c: 0, k: 4} : i % w === 4 ? {d: 0, k: 5} : {e: 0, f: 0, k: 6}
		s += read(o)
	}
	return s
}
function readG(o) { return o.k }
const G = {get k() { return 7 }}
function got(n) { let s = 0; for (let i = 0; i < n; i++) s += readG(i & 1 ? Object.create(G) : {k: 1}); return s }
function readD(o) { return o.k }
const P4 = {k: 9}, P3 = Object.create(P4), P2 = Object.create(P3), P1 = Object.create(P2)
function deep(n, m) { let s = 0; for (let i = 0; i < n; i++) s += readD(i % m === 0 ? Object.create(P1) : {k: 1}); return s }
function readP(v) { return v.x }
function prim(n, m) { let s = 0; for (let i = 0; i < n; i++) { const v = readP(i % m === 0 ? 1.5 : {x: 1}); s += v === undefined ? 0 : v } return s }
const Q3 = {}, Q2 = Object.create(Q3), Q1 = Object.create(Q2)
function add(n) { let s = 0; for (let i = 0; i < n; i++) { const o = i & 1 ? Object.create(Q1) : {}; o.z = i; s += o.z } return s }
for (let k = 0; k < 30; k++) many(300, 1)
for (let k = 0; k < 30; k++) many(300, 3)
for (let k = 0; k < 30; k++) many(300, 6)
for (let k = 0; k < 30; k++) got(300)
for (let k = 0; k < 30; k++) deep(300, 1000)
for (let k = 0; k < 30; k++) deep(300, 2)
for (let k = 0; k < 30; k++) prim(300, 1000)
for (let k = 0; k < 30; k++) prim(300, 2)
for (let k = 0; k < 30; k++) add(300)
for (let k = 0; k < 30; k++) many(300, 6)
`

// jitPolyBytes is what of a list of learned property sites a snapshot reads,
// byte for byte: each site's own fields, its lists' headers among them,
// and the elements of each of its lists up to their lengths.
func jitPolyBytes(sites []jitPolySite) []byte {
	var b bytes.Buffer
	raw := func(p unsafe.Pointer, n uintptr) {
		if n != 0 {
			b.Write(unsafe.Slice((*byte)(p), n))
		}
	}
	for i := range sites {
		p := &sites[i]
		raw(unsafe.Pointer(p), unsafe.Sizeof(*p))
		raw(unsafe.Pointer(unsafe.SliceData(p.cases)), uintptr(len(p.cases))*unsafe.Sizeof(propCache{}))
		raw(unsafe.Pointer(unsafe.SliceData(p.reads)), uintptr(len(p.reads))*unsafe.Sizeof(jitDeepRead{}))
		raw(unsafe.Pointer(unsafe.SliceData(p.adds)), uintptr(len(p.adds))*unsafe.Sizeof(jitDeepAdd{}))
		raw(unsafe.Pointer(unsafe.SliceData(p.accessors)), uintptr(len(p.accessors))*unsafe.Sizeof(jitAccessorCase{}))
		raw(unsafe.Pointer(unsafe.SliceData(p.missed)), uintptr(len(p.missed))*unsafe.Sizeof(jitMissed{}))
	}
	return slices.Clone(b.Bytes())
}

// TestJITLearnedPolyPersists holds the learned property sites a snapshot
// takes (jitPolyTake), at every compile asked, to staying byte for byte as
// they were when taken, whatever the exits write after: every write of a
// site goes through jitPolySiteAt, which makes the list anew while a
// snapshot holds it, and the one rewrite of a site's list in place,
// jitAddReadCase's, makes that list anew. A writer that wrote a site in
// place had changed what the worker reads under it. The workload's exits
// write every kind a site keeps, which the test checks they reached, and
// the rewrite is held so too, on a site made for it.
func TestJITLearnedPolyPersists(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	type taken struct {
		e     *jitEntry
		sites []jitPolySite
		bytes []byte
	}
	var all []taken
	r := jitRuntimeForTest(t, Config{JIT: true})
	jitTestSwitch(t, func(c *jitTestConfig) {
		c.compileNote = func(r *Runtime, _ *closure) {
			for _, e := range r.jitState().regLearned {
				if sites := jitPolyTake(e); len(sites) != 0 {
					all = append(all, taken{e, sites, jitPolyBytes(sites)})
				}
			}
		}
	})
	if _, err := r.Run(compileForTest(t, jitPolyPersistSrc)); err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no learned property sites taken")
	}
	moved := 0
	for _, x := range all {
		if got := jitPolyBytes(x.sites); !bytes.Equal(got, x.bytes) {
			t.Errorf("learned property sites changed under the snapshot holding them: %d sites", len(x.sites))
		}
		if x.e.jitLearned != nil && unsafe.SliceData(x.e.poly) != unsafe.SliceData(x.sites) {
			moved++
		}
	}
	// The kinds the exits wrote, each of which the test reaches.
	var cases, reads, adds, accessors, missed, mega, prim bool
	for _, e := range r.jitState().regLearned {
		if e.jitLearned == nil {
			continue
		}
		for i := range e.poly {
			p := &e.poly[i]
			cases, reads, adds = cases || len(p.cases) != 0, reads || len(p.reads) != 0, adds || len(p.adds) != 0
			accessors, missed = accessors || len(p.accessors) != 0, missed || len(p.missed) != 0
			mega, prim = mega || p.mega, prim || p.prim
		}
	}
	t.Logf("%d lists taken, %d made anew since", len(all), moved)
	if moved == 0 {
		t.Error("no list taken was made anew: the test does not reach a write while one is held")
	}
	for name, ok := range map[string]bool{"cases": cases, "reads": reads, "adds": adds, "accessors": accessors, "missed": missed, "mega": mega, "prim": prim} {
		if !ok {
			t.Errorf("no site learned %s: the test does not reach its writes", name)
		}
	}

	// The rewrite: a site's cases full, a new case whose shape came from
	// one of theirs takes its place (jitAddReadCase).
	var shapes [jitPropertyCases]*shape
	e := &jitEntry{jitLearned: &jitLearned{}}
	p := jitPolySiteAt(e, 7)
	for i := range shapes {
		shapes[i] = &shape{}
		p.cases = append(p.cases, propCache{shape: shapes[i]})
	}
	held := jitPolyTake(e)
	before := jitPolyBytes(held)
	if !jitAddReadCase(jitPolySiteAt(e, 7), propCache{shape: &shape{parent: shapes[0]}}, nil) {
		t.Fatal("the rewrite of a full site's cases was not made")
	}
	if !bytes.Equal(jitPolyBytes(held), before) {
		t.Error("a site's cases rewritten in place under the snapshot holding them")
	}
	if e.poly[0].cases[0].shape == shapes[0] {
		t.Error("the rewrite left its first case")
	}
}
