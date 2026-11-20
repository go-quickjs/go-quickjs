package vm

import "testing"

// A collection's storage follows what it holds, not everything it has ever
// held: a Map used as a queue, or a Set cleared again and again, keeps a list
// and an index the size of its live entries. Deleted entries used to stay in
// the list for good, so iterating an empty Map walked every key it had had,
// and clear, which marked each of them again, made a Set cleared after every
// few additions quadratic.
func TestMapStorageBounded(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})

	queue := newJSMap()
	for i := 0; i < 100000; i++ {
		queue.set(r, Int(i), Int(i))
		if i >= 10 {
			if !queue.delete(Int(i - 10)) {
				t.Fatalf("delete %d found nothing", i-10)
			}
		}
	}
	if queue.size != 10 || len(queue.entries) > 64 || cap(queue.entries) > 256 || len(queue.index) > 256 {
		t.Errorf("queue of 10: size %d, entries %d (cap %d), index %d",
			queue.size, len(queue.entries), cap(queue.entries), len(queue.index))
	}
	for i := 99990; i < 100000; i++ {
		if v, ok := queue.get(r, Int(i)); !ok || v.Number() != float64(i) {
			t.Fatalf("get %d = %v %v", i, v, ok)
		}
	}

	set := newJSMap()
	for i := 0; i < 100000; i++ {
		k := Str(NewString(string(rune('a' + i%10))))
		set.set(r, k, k)
		if i%10 == 9 {
			set.clear()
		}
	}
	if set.size != 0 || cap(set.entries) > 64 || len(set.index) > 64 {
		t.Errorf("cleared set: size %d, entries cap %d, index %d", set.size, cap(set.entries), len(set.index))
	}

	// A map that grew large and then emptied gives the room back.
	big := newJSMap()
	for i := 0; i < 50000; i++ {
		big.set(r, Int(i), Undefined)
	}
	for i := 0; i < 50000; i++ {
		big.delete(Int(i))
	}
	if big.size != 0 || cap(big.entries) > 64 || len(big.index) > 64 {
		t.Errorf("emptied map: size %d, entries cap %d, index %d", big.size, cap(big.entries), len(big.index))
	}
}

// A string key is hashed by its text, whatever made the string: a key of
// every length up to past maphash's, each made as a flat string, a rope
// and a slice of a longer one, finds the entry its equal made, through the
// rebuilds a map filled from empty makes and those its deletions make.
func TestMapStringKeysByText(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const bad = []
		const text = n => { let s = ''; for (let i = 0; i < n; i++) s += String.fromCharCode(97 + (i * 7 + n) % 26); return s }
		const m = new Map()
		for (let n = 0; n <= 40; n++) m.set(text(n), n)
		for (let n = 0; n <= 40; n++) {
			const t = text(n), h = n >> 1
			const forms = [t, t.slice(0, h) + t.slice(h), ('#' + t + '#').slice(1, n + 1), [...t].join('')]
			for (const f of forms) if (m.get(f) !== n) bad.push(n + ': ' + JSON.stringify(f) + ' -> ' + m.get(f))
		}
		for (let n = 0; n <= 40; n += 2) m.delete(text(n))
		for (let n = 0; n <= 40; n++) if (m.has(text(n)) !== (n % 2 === 1)) bad.push('after deletes ' + n)
		const u = new Map([['\u00e9t\u00e9', 1], ['\ud83d\ude00', 2], ['caf\u00e9 au lait \u2615', 3]])
		if (u.get('\u00e9' + 't\u00e9') !== 1 || u.get('\ud83d' + '\ude00') !== 2 || u.get('caf\u00e9 au lait ' + '\u2615') !== 3) bad.push('non-ASCII')
		bad.push(m.size)
		bad.join(';')`))
	if err != nil {
		t.Fatal(err)
	}
	if s := v.String().Go(); s != "20" {
		t.Fatalf("got %q, want %q", s, "20")
	}
}

// Keys equal as strings hash alike, and the short path spreads the keys a
// script makes of numbers over the index as a random function would:
// 100,000 keys into 65,536 places fill 1 - e^(-100000/65536) of them,
// 51,290.
func TestStrKeyHash(t *testing.T) {
	seen := map[uint32]bool{}
	for i := 0; i < 100000; i++ {
		s := Str(NewString(itoaForTest(i)))
		h := keyHash(s)
		if h != keyHash(Str(NewString(itoaForTest(i)))) {
			t.Fatalf("%d hashed two ways", i)
		}
		seen[h&0xFFFF] = true
	}
	if len(seen) < 50500 {
		t.Errorf("100,000 keys took %d of 65,536 low halves", len(seen))
	}
}

func itoaForTest(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
