package vm

import "testing"

// TestSlackOutgrown pins the room a site's next object is made with, once
// its objects have grown: the room they came to need while that is at most
// maxSlack, and none past it. A site whose objects outgrew maxSlack gave
// each of them maxSlack's room, which every one carried in its own
// allocation and outgrew: an object of twenty properties built as a tree
// held a third more than the interpreter's.
func TestSlackOutgrown(t *testing.T) {
	defer SetTreeTier(true)
	SetTreeTier(true)
	for _, tt := range []struct {
		name, src string
		room      int
		inline    bool
	}{
		{"literal of six", `function f(fill) { let o = {}; if (fill) { o.a = 1; o.b = 2; o.c = 3; o.d = 4; o.e = 5; o.f = 6 } return o }`, 6, true},
		{"literal of twenty", `function f(fill) { let o = {}; if (fill) for (let k = 0; k < 20; k++) o["p" + k] = k; return o }`, 3, true},
		{"constructor of six", `function C(fill) { if (fill) { this.a = 1; this.b = 2; this.c = 3; this.d = 4; this.e = 5; this.f = 6 } } function f(fill) { return new C(fill) }`, 6, true},
		{"constructor of twenty", `function C(fill) { if (fill) for (let k = 0; k < 20; k++) this["p" + k] = k } function f(fill) { return new C(fill) }`, 3, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := New(Config{})
			defer r.Close()
			v, err := r.Run(compileForTest(t, tt.src+`; for (let i = 0; i < 8; i++) f(true); f(false)`))
			if err != nil {
				t.Fatal(err)
			}
			o := v.Object()
			if room, inline := cap(o.props), o.flags&objInlineProps != 0; room != tt.room || inline != tt.inline {
				t.Errorf("made with room for %d (inline %v), want %d (inline %v)", room, inline, tt.room, tt.inline)
			}
		})
	}
}
