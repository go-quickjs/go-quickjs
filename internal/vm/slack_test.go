package vm

import (
	"fmt"
	"strings"
	"testing"
)

// TestSlackOutgrown pins the room a site's next object is made with, once
// its objects have grown: the room they came to need, in the object's own
// allocation while that is at most maxSlack, and past it a table of its
// own made at that size, which no object carries and outgrows; a
// constructor's, once its first objects are built, what they used (slack
// tracking). Room for maxSlack had been carried by every object of a site
// whose objects outgrew it, and outgrown: an object of twenty properties
// built as a tree held a third more than the interpreter's.
func TestSlackOutgrown(t *testing.T) {
	defer SetTreeTier(true)
	SetTreeTier(true)
	for _, tt := range []struct {
		name, src string
		room      int
		inline    bool
	}{
		{"literal of six", `function f(fill) { let o = {}; if (fill) { o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6 } return o }`, 6, true},
		{"literal of twenty", `function f(fill) { let o = {}; if (fill) for (let k = 0; k < 20; k++) o["p" + k] = k; return o }`, 9, false},
		{"constructor of six", `function C(fill) { if (fill) { this.a = 1; this.b = 2; this.c = 3; this.d = 4; this.e = 5; this.f = 6 } } function f(fill) { return new C(fill) }`, 6, true},
		{"constructor of twenty", `function C(fill) { if (fill) for (let k = 0; k < 20; k++) this["p" + k] = k } function f(fill) { return new C(fill) }`, 20, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			v, err := r.Run(compileForTest(t, tt.src+`; for (let i = 0; i < 8; i++) f(true); f(false)`))
			if err != nil {
				t.Fatal(err)
			}
			o := v.Object()
			if room, inline := cap(o.vals), o.flags&objInlineProps != 0; room != tt.room || inline != tt.inline {
				t.Errorf("made with room for %d (inline %v), want %d (inline %v)", room, inline, tt.room, tt.inline)
			}
		})
	}
}

// A constructor's objects are made with room for the properties its body
// assigns to this by name, as many as V8 keeps in an object (maxRoom):
// Gameboy's emulator, of 215, had been made with room for 16 and grown
// four times. A body that names more than its objects get -- a property in
// each branch of a switch -- has its objects made with what its first
// slackTracking used, as V8's slack tracking shrinks a map's instance
// size.
func TestSlackTracking(t *testing.T) {
	var big strings.Builder
	big.WriteString("function Big() {")
	for i := 0; i < 215; i++ {
		fmt.Fprintf(&big, " this.p%d = %d;", i, i)
	}
	big.WriteString(" }")
	var sw strings.Builder
	sw.WriteString("function Sw(k) { this.k = k; switch (k % 40) {")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sw, " case %d: this.q%d = %d; break;", i, i, i)
	}
	sw.WriteString(" } }")
	r := New(Config{})
	defer r.Close()
	room := func(src string) int {
		t.Helper()
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		return cap(v.Object().vals)
	}
	if _, err := r.Run(compileForTest(t, big.String()+"; "+sw.String())); err != nil {
		t.Fatal(err)
	}
	if n := room(`new Big()`); n != 215 {
		t.Errorf("an object of 215 named properties made with room for %d", n)
	}
	if n := room(`new Sw(0)`); n != 41 {
		t.Errorf("before tracking, Sw's object made with room for %d, want the 41 its body names", n)
	}
	if n := room(`for (let i = 1; i < 10; i++) new Sw(i); new Sw(3)`); n != 2 {
		t.Errorf("after tracking, Sw's object made with room for %d, want the 2 its objects used", n)
	}
}
