package quickjs_test

import (
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestSourceURL pins //# sourceURL= as V8 reads it: the code of an eval or
// a Function call that names itself is called by that name in a stack
// trace, with no word of where it was evaluated; //@ is as good as //#,
// whitespace around the name is dropped, a name with whitespace in it or a
// comment that is no single-line one names nothing, nor does the text of a
// string, and the last one counts. A CallSite's getFileName says nothing
// of it, and its getScriptNameOrSourceURL gives it. Node v26.10's are the
// answers, ORIGIN where the eval's origin names this test's file.
func TestSourceURL(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	src := "const top = (f) => { try { f(); } catch (e) { return e.stack.split(\"\\n\")[1].trim(); } };\nconst out = [];\nout.push(top(() => eval(\"throw new Error('a');\\n//# sourceURL=thrower.js\")));\nout.push(top(() => eval(\"throw new Error('a');\\n//@ sourceURL=old.js\")));\nout.push(top(() => eval(\"throw new Error('a');\\n//# sourceURL=  spaced.js  \")));\nout.push(top(() => eval(\"//# sourceURL=first.js\\nthrow new Error('a');\")));\nout.push(top(() => new Function(\"throw new Error('a');\\n//# sourceURL=fn.js\")()));\nout.push(top(() => eval(\"throw new Error('a');\\n//# sourceURL=\\\"q.js\\\"\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nout.push(top(() => eval(\"throw new Error('a');\\n//# sourceURL=a b.js\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nout.push(top(() => eval(\"var s = '//# sourceURL=fake.js'; throw new Error('a');\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nout.push(top(() => eval(\"throw new Error('a');\\n/*# sourceURL=blk.js */\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nout.push(top(() => eval(\"throw new Error('a');\\n//#sourceURL=nospace.js\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nout.push(top(() => eval(\"//# sourceURL=one.js\\nthrow new Error('a');\\n//# sourceURL=two.js\")));\nout.push(top(() => eval(\"throw new Error('a'); //# sourceURL=trailing.js\")).replace(/\\(.*?, /, \"(ORIGIN, \"));\nconst prev = Error.prepareStackTrace;\nError.prepareStackTrace = (e, cs) => [cs[0].getFileName(), cs[0].isEval(), cs[0].getEvalOrigin() === undefined ? \"no origin\" : \"origin\", cs[0].getScriptNameOrSourceURL()].join(\" \");\ntry { eval(\"throw new Error('a');\\n//# sourceURL=cs.js\"); } catch (e) { out.push(e.stack); }\nError.prepareStackTrace = prev;\nconsole.log(out.join(\"\\n\"));\n"
	var out []string
	if err := rt.Set("report", func(s string) { out = append(out, s) }); err != nil {
		t.Fatal(err)
	}
	src = strings.Replace(src, "console.log(out.join(", "report(out.join(", 1)
	if _, err := rt.EvalFile("surl.js", src); err != nil {
		t.Fatal(err)
	}
	want := "at eval (thrower.js:1:7)\nat eval (old.js:1:7)\nat eval (spaced.js:1:7)\nat eval (first.js:2:7)\nat eval (fn.js:3:7)\nat eval (\"q.js\":1:7)\nat eval (ORIGIN, <anonymous>:1:7)\nat eval (ORIGIN, <anonymous>:1:40)\nat eval (ORIGIN, <anonymous>:1:7)\nat eval (ORIGIN, <anonymous>:1:7)\nat eval (two.js:2:7)\nat eval (trailing.js:1:7)\n true origin cs.js"
	if got := strings.Join(out, ""); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
