package vm

import (
	"fmt"
	"hash/maphash"
	"testing"
)

// memoSlotsApart says the texts take slots of their own in the memo, whose
// seed is the process's: when two share a slot, each join of one evicts
// the other's string.
func memoSlotsApart(texts ...string) bool {
	seen := map[uint64]bool{}
	for _, s := range texts {
		at := maphash.String(mapSeed, s) & (concatMemoSlots - 1)
		if seen[at] {
			return false
		}
		seen[at] = true
	}
	return true
}

// concatMemoSrc joins the same parts over and over, as a tree's leaves do
// ('String for key ' + tag + ' in leaf node' for every leaf of one tag), and
// texts that differ only in where a surrogate's half is: a join's flags
// (startsLow, endsHigh) are its parts', and a string the memo holds is
// returned only for a join that would have made the same.
const concatMemoSrc = `
function leaves(tags, n) {
  const out = []
  for (const tag of tags)
    for (let i = 0; i < n; i++) out.push('String for key ' + tag + ' in leaf node')
  return out
}
function numbers(n) {
  const out = []
  for (let i = 0; i < n; i++) out.push(` + "`key-${i % 4}`" + `)
  return out
}
function halves() {
  const hi = '\uD83D', lo = '\uDE00', out = []
  for (let i = 0; i < 4; i++) {
    out.push('a' + hi + '-', 'a' + hi + lo, '-' + lo + 'b', hi + lo + '-')
    out.push(` + "`${hi}${lo}`" + `, ` + "`x${hi}`" + `, ` + "`${lo}x`" + `)
  }
  return out.map(s => s.length + ':' + [...s].length + ':' + s.codePointAt(1) + ':' + encodeURIComponent(s.replace(/[\uD800-\uDFFF]/g, '?')))
}
`

func TestConcatMemo(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		if _, err := r.Run(compileForTest(t, concatMemoSrc)); err != nil {
			t.Fatal(err)
		}
		v, err := r.Run(compileForTest(t, `leaves(['1234.5678', 'abc', '1234.5678'], 40)`))
		if err != nil {
			t.Fatal(err)
		}
		elems := v.Object().elemsOf()
		distinct := map[*String]bool{}
		for i, e := range elems {
			tag := []string{"1234.5678", "abc", "1234.5678"}[i/40]
			if got, want := e.String().Go(), "String for key "+tag+" in leaf node"; got != want {
				t.Fatalf("tree %v: leaf %d is %q, want %q", tree, i, got, want)
			}
			distinct[e.String()] = true
		}
		// One string for each text: the memo made each once.
		if len(distinct) != 2 && memoSlotsApart("String for key 1234.5678 in leaf node", "String for key abc in leaf node") {
			t.Errorf("tree %v: %d distinct strings for 2 texts", tree, len(distinct))
		}
		v, err = r.Run(compileForTest(t, `numbers(40)`))
		if err != nil {
			t.Fatal(err)
		}
		distinct = map[*String]bool{}
		for i, e := range v.Object().elemsOf() {
			if got, want := e.String().Go(), fmt.Sprintf("key-%d", i%4); got != want {
				t.Fatalf("tree %v: key %d is %q, want %q", tree, i, got, want)
			}
			distinct[e.String()] = true
		}
		if len(distinct) != 4 && memoSlotsApart("key-0", "key-1", "key-2", "key-3") {
			t.Errorf("tree %v: %d distinct strings for 4 texts", tree, len(distinct))
		}
		v, err = r.Run(compileForTest(t, `halves().join(' ')`))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		const once = "3:3:55357:a%3F- 3:2:128512:a%3F%3F 3:3:56832:-%3Fb 3:2:56832:%3F%3F- 2:1:56832:%3F%3F 2:2:55357:x%3F 2:2:120:%3Fx"
		want := once
		for range 3 {
			want += " " + once
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: halves\n got %s\nwant %s", tree, got, want)
		}
	}
	SetTreeTier(true)
}

// A runtime whose joins never repeat turns its memo off, its strings
// dropped, and on again after concatMemoOff joins.
func TestConcatMemoTurnsOff(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
for (let i = 0; i < 5000; i++) globalThis.last = 'k' + i + 'v'
`)); err != nil {
		t.Fatal(err)
	}
	m := r.concatMemo
	if m == nil || m.off == 0 {
		t.Fatalf("memo not off after joins that never repeat: %+v", m)
	}
	for _, s := range m.slots {
		if s != nil {
			t.Fatal("memo off but still keeps strings")
		}
	}
	off := m.off
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 10; i++) globalThis.last = 'k' + i + 'v'`)); err != nil {
		t.Fatal(err)
	}
	if m.off != off-10 {
		t.Errorf("off went from %d to %d over 10 joins", off, m.off)
	}
}

// Under a memory limit joins are charged as they always were, and the
// memo is not used.
func TestConcatMemoNotMetered(t *testing.T) {
	r := New(Config{})
	r.setMemoryLimit(1 << 30)
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `for (let i = 0; i < 100; i++) globalThis.last = 'k' + (i & 1) + 'v'`)); err != nil {
		t.Fatal(err)
	}
	if r.concatMemo != nil {
		t.Error("memo made under a memory limit")
	}
}

// A memo hit is a string whose whole text is the join's, not one whose text
// falls in the same slot: a slot holding another text of the same length,
// or one differing in its last byte, is replaced, never returned.
func TestConcatMemoCollision(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `function j(a) { return 'pre-' + a + '-post' }`)); err != nil {
		t.Fatal(err)
	}
	const want = "pre-abc-post"
	at := maphash.String(mapSeed, want) & (concatMemoSlots - 1)
	for _, other := range []string{"pre-abd-post", "pre-abc-posu", "PRE-ABC-POST", "pre-abc-pos", "x"} {
		if r.concatMemo == nil {
			r.concatMemo = new(concatMemo)
		}
		impostor := NewString(other)
		r.concatMemo.slots[at] = impostor
		v, err := r.Run(compileForTest(t, `j('abc')`))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got == impostor || got.Go() != want {
			t.Fatalf("slot holding %q: join gave %q (impostor %v)", other, got.Go(), got == impostor)
		}
		if s := r.concatMemo.slots[at]; s == impostor || s.Go() != want {
			t.Errorf("slot holding %q: not replaced by the join's string", other)
		}
	}
	// The same text with other flags is not the join's either.
	odd := NewString(want)
	odd.ascii = false
	r.concatMemo.slots[at] = odd
	v, err := r.Run(compileForTest(t, `j('abc')`))
	if err != nil {
		t.Fatal(err)
	}
	if v.String() == odd || !v.String().ascii {
		t.Error("join returned a string whose flags differ from its own")
	}
}
