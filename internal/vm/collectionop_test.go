package vm

import "testing"

// TestCollectionOpWithoutFrame pins Map.prototype.get, has and set and
// Set.prototype.has and add called from a script, which on a Map or a Set
// are applied without a native frame (collectionOp), from a pure body too:
// keys as SameValueZero has them -- -0 as 0, NaN, a string built by
// concatenation, an object -- set and add returning their receiver, a
// subclass's instance, and the TypeError another receiver gets from the
// built-in. The output is Node's.
func TestCollectionOpWithoutFrame(t *testing.T) {
	const src = `var out = [];
function has(m, k) { return m.has(k) }
function get(m, k) { return m.get(k) }
function put(m, k, v) { return m.set(k, v) }
function run() {
	var m = new Map(), s = new Set();
	for (var i = 0; i < 20; i++) { m.set(i, i * 2); s.add("k" + i) }
	out.push(m.get(3), m.has(19), m.has(20), m.get(20), s.has("k5"), s.has("k20"), m.size, s.size);
	out.push(m.set(-0, "zero") === m, Object.is(m.keys().next().value, 0), m.get(0), m.get(-0));
	m.set(NaN, "nan");
	out.push(m.get(NaN), m.has(0 / 0), s.add(-0) === s, s.has(0));
	var rope = "ab";
	for (var j = 0; j < 5; j++) rope += "cd";
	m.set(rope, "rope");
	out.push(m.get("abcdcdcdcdcd"), has(m, "abcdcdcdcdcd"), get(m, 3), put(m, "p", 1) === m, m.get("p"));
	try { Map.prototype.get.call(s, 1) } catch (e) { out.push(e instanceof TypeError) }
	try { Set.prototype.add.call(m, 1) } catch (e) { out.push(e instanceof TypeError) }
	try { m.has.call({}, 1) } catch (e) { out.push(e instanceof TypeError) }
	class M extends Map {}
	var sub = new M();
	sub.set("x", 1);
	out.push(sub.get("x"), sub.has("x"), sub instanceof M);
	var o = {};
	var wm = new Map([[o, "obj"]]);
	out.push(wm.get(o), wm.get({}), has(wm, o));
	out.push(JSON.stringify([...m.keys()].slice(-3)), JSON.stringify([...s].slice(-2)));
}
for (var k = 0; k < 3; k++) run();
JSON.stringify(out);
`
	const want = `[6,true,false,null,true,false,20,20,true,true,"zero","zero","nan",true,true,true,"rope",true,6,true,1,true,true,true,1,true,true,"obj",null,true,"[null,\"abcdcdcdcdcd\",\"p\"]","[\"k19\",0]",6,true,false,null,true,false,20,20,true,true,"zero","zero","nan",true,true,true,"rope",true,6,true,1,true,true,true,1,true,true,"obj",null,true,"[null,\"abcdcdcdcdcd\",\"p\"]","[\"k19\",0]",6,true,false,null,true,false,20,20,true,true,"zero","zero","nan",true,true,true,"rope",true,6,true,1,true,true,true,1,true,true,"obj",null,true,"[null,\"abcdcdcdcdcd\",\"p\"]","[\"k19\",0]"]`
	defer SetTreeTier(true)
	for _, tree := range []bool{false, true} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, src))
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != want {
			t.Errorf("tree %v: got %s, want %s", tree, got, want)
		}
		r.Close()
	}
}
