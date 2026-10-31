package vm

import "testing"

// A read whose own cache has given up is answered by the runtime's shared
// cache (sharedProp), of entries in pairs: reads of own properties and of
// prototypes' at one and two levels, across forty layouts, stay right as
// a prototype's method is replaced, a property is added to a prototype in
// front of the one that had it, and one is deleted -- in the interpreter
// and the trees alike.
func TestSharedCacheMegamorphic(t *testing.T) {
	src := `
		function Base() {} Base.prototype.kind = function () { return "base"; };
		var classes = [];
		for (var c = 0; c < 40; c++) {
			(function (c) {
				function K() { this.id = c; for (var k = 0; k < c % 7; k++) this["f" + k] = k; }
				K.prototype = Object.create(Base.prototype);
				if (c % 5 == 0) K.prototype.kind = function () { return "k" + c; };
				classes.push(K);
			})(c);
		}
		var objs = [];
		for (var i = 0; i < 120; i++) objs.push(new classes[i % 40]());
		function sum() { var s = ""; for (var i = 0; i < objs.length; i++) { var o = objs[i]; s += o.id + o.kind() + (o.f2 === undefined ? "-" : o.f2) + ";"; } return s; }
		var out = [];
		for (var r = 0; r < 30; r++) sum();
		out.push(sum());
		Base.prototype.kind = function () { return "B"; };
		out.push(sum());
		classes[3].prototype.kind = function () { return "three"; };
		out.push(sum());
		delete classes[5].prototype.kind;
		out.push(sum());
		var h = 0;
		for (var j = 0; j < out.length; j++) for (var k = 0; k < out[j].length; k++) h = (h * 31 + out[j].charCodeAt(k)) | 0;
		[out[0].slice(0, 40), out[1].slice(0, 40), out[2].slice(0, 60), out[3].slice(0, 80), h].join("|")`
	defer SetTreeTier(true)
	var answers []string
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		answers = append(answers, v.String().Go())
	}
	// Node's answer, and the interpreter's before the cache was of pairs.
	const want = "0k0-;1base-;2base-;3base2;4base2;5k52;6b|0k0-;1B-;2B-;3B2;4B2;5k52;6B2;7B-;8B-;9B|0k0-;1B-;2B-;3three2;4B2;5k52;6B2;7B-;8B-;9B-;10k102;11B2;12|0k0-;1B-;2B-;3three2;4B2;5B2;6B2;7B-;8B-;9B-;10k102;11B2;12B2;13B2;14B-;15k15-;1|-1921851682"
	for i, got := range answers {
		if len(got) < len(want) || got[:len(want)] != want {
			t.Errorf("tree tier %v: got %s", i == 1, got)
		}
	}
	if answers[0] != answers[1] {
		t.Errorf("the tiers differ:\n%s\n%s", answers[0], answers[1])
	}
}
