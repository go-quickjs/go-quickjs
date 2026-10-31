package vm

import "testing"

// A property of an upvalue is read in place (getPropOperand): an own
// property, one a prototype holds, a getter's, the upvalue given an object
// of another layout, a string, null -- whose read throws a TypeError. Each
// answer is the interpreter's, and the trees build the read.
func TestTreeUpvalueProperty(t *testing.T) {
	src := `
		function make(o) {
			function read(n) { var s = ''; for (var i = 0; i < n; i++) s += o.x + ',' + o.y + ';'; return s }
			return { read: read, set: function (v) { o = v } }
		}
		var p = { y: 'proto' }, out = [];
		var m = make(Object.assign(Object.create(p), { x: 1 }));
		for (var i = 0; i < 3; i++) out.push(m.read(3));
		m.set({ x: 'other', y: 2, z: 3 }); out.push(m.read(2));
		m.set(Object.defineProperty({}, 'x', { get: function () { return 'got' } })); out.push(m.read(2));
		m.set('str'); out.push(m.read(1));
		m.set(null);
		try { m.read(1) } catch (e) { out.push(e.constructor.name) }
		out.join(' | ')`
	var want string
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		got := v.String().Go()
		if !tree {
			want = got
			continue
		}
		if got != want {
			t.Errorf("the trees gave\n%s\nthe interpreter\n%s", got, want)
		}
	}
	if want == "" || want[len(want)-9:] != "TypeError" {
		t.Errorf("the interpreter gave %q", want)
	}
}
