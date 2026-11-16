package vm

import "testing"

// A reviver that cannot see a third argument -- two parameters, no rest
// parameter, no arguments object -- is called with two,
// and the parse keeps no records of the source text for it; one that can
// is given the context, its source of a primitive as before. Each answer
// is Node's: the holder as this, the keys in order, the values revived
// bottom-up, a value deleted by undefined.
func TestJSONReviverThirdArgument(t *testing.T) {
	t.Parallel()
	const src = `
		var text = '{"a": 1, "b": [2, "x", true], "c": {"d": null}}', log = []
		function two(k, v) { log.push(k + ":" + (this === undefined ? "u" : typeof this)); return k === "d" ? undefined : v }
		var r1 = JSON.stringify(JSON.parse(text, two))
		var arrow = (k, v) => typeof v === "number" ? v * 10 : v
		var r2 = JSON.stringify(JSON.parse(text, arrow))
		function args(k, v) { return typeof v === "object" ? v : arguments.length + "/" + arguments[2].source }
		var r3 = JSON.stringify(JSON.parse(text, args))
		var r4 = JSON.stringify(JSON.parse(text, function (k, v, ...rest) { return typeof v === "object" ? v : rest.length + rest[0].source }))
		var r5 = JSON.stringify(JSON.parse(text, function (k, v, c) { return typeof v === "object" ? v : c.source }))
		var r7 = JSON.stringify(JSON.parse("[1,2,3]", function (k, v) { return Array.isArray(this) && k !== "" ? this.length + v : v }))
		;[log.join(), r1, r2, r3, r4, r5, r7].join("|")`
	const want = `a:object,0:object,1:object,2:object,b:object,d:object,c:object,:object|` +
		`{"a":1,"b":[2,"x",true],"c":{}}|{"a":10,"b":[20,"x",true],"c":{"d":null}}|` +
		`{"a":"3/1","b":["3/2","3/\"x\"","3/true"],"c":{"d":null}}|` +
		`{"a":"11","b":["12","1\"x\"","1true"],"c":{"d":null}}|` +
		`{"a":"1","b":["2","\"x\"","true"],"c":{"d":null}}|` +
		`[4,5,6]`
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
