package vm

import "testing"

// An ASCII string matched against a pattern -- by each method that takes
// one -- is matched by its bytes and is not given code units, which would be
// twice its bytes for as long as it lived; one that is not ASCII is.
func TestRegExpASCIISubjectKeepsNoUnits(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	if _, err := r.Run(compileForTest(t, `
		var names = ["exec", "test", "match", "matchAll", "search", "split", "replace", "replaceFn", "replace$", "sticky"]
		var subjects = names.map(n => n + " subject " + "x".repeat(40)), wide = "wide é subject"
		var fns = [s => /s(u)b/.exec(s), s => /sub/g.test(s), s => s.match(/\w+/g), s => [...s.matchAll(/b/g)],
			s => s.search(/ject/), s => s.split(/ /), s => s.replace(/s/g, "t"), s => s.replace(/s/g, m => m),
			s => s.replace(/(s)/g, "$1$1"), s => /x+/y.exec(s)]
		subjects.forEach((s, i) => fns[i](s)); /é/.test(wide)`)); err != nil {
		t.Fatal(err)
	}
	get := func(name string) Value { return r.global.getOwn(r.atoms.intern(name)).value() }
	subjects := get("subjects").Object()
	for i, v := range subjects.elemsOf() {
		if s := v.String(); s.units16() != nil {
			t.Errorf("subject %d, %q: given code units", i, s.Go())
		}
	}
	if get("wide").String().units16() == nil {
		t.Error("a subject not ASCII was matched without its code units")
	}
}
