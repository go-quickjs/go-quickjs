package vm

import "testing"

// A collection's storage follows what it holds, not everything it has ever
// held: a Map used as a queue, or a Set cleared again and again, keeps a list
// and an index the size of its live entries. Deleted entries used to stay in
// the list for good, so iterating an empty Map walked every key it had had,
// and clear, which marked each of them again, made a Set cleared after every
// few additions quadratic.
func TestMapStorageBounded(t *testing.T) {
	r := New(Config{})

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
