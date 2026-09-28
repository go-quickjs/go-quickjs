package quickjs_test

import "testing"

// TestRegExpsShareCompiledPatterns pins that RegExps made from the same
// pattern, which share its compiled program, share nothing a script can see:
// each has its own lastIndex, recompiling one leaves the other as it was, a
// pattern used again while it is matching -- in a replacement callback -- is
// matched afresh, and an overridden exec is still called. And that
// instanceof, whose default Symbol.hasInstance is called with an argument
// list the runtime keeps, answers through a chain of bound functions.
func TestRegExpsShareCompiledPatterns(t *testing.T) {
	for src, want := range map[string]string{
		`var a = /a/g, b = /a/g; a.lastIndex = 1; b.test("aa"); JSON.stringify([a.lastIndex, b.lastIndex])`:                    "[1,1]",
		`var c = /x/, d = /x/; c.compile("y"); JSON.stringify([c.source, d.source, d.test("x"), c.test("x")])`:                 `["y","x",true,false]`,
		`"aXbX".replace(/X/g, () => "aXa".replace(/X/g, "-"))`:                                                                 "aa-aba-a",
		`function mk() { return /(\d+)-(\d+)/g } var s = "1-2 33-44"; s.replace(mk(), (m, x, y) => s.replace(mk(), "$2") + y)`: "2 442 2 4444",
		`var e = /q/; e.exec = function (s) { return null }; "q".replace(e, "Z") + " " + /q/.test("q")`:                        "q true",
		`function F() {} var B = F.bind(null).bind(null); [new F() instanceof B, {} instanceof B, [] instanceof Array].join()`: "true,false,true",
		`for (var i = 0, n = 0; i < 300; i++) if (new RegExp("p" + (i % 5)).test("p" + (i % 5))) n++; n`:                       "300",
		`[/a/.source === /a/.source, new RegExp("").source, /\//.source].join(" ")`:                                            `true (?:) \/`,
	} {
		checkEval(t, src, want)
	}
}
