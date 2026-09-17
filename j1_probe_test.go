package quickjs_test

import (
	"fmt"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

func TestJ1Probe(t *testing.T) {
	mods := map[string]string{
		"setup": `export const p1 = Promise.withResolvers()
			export const pA = Promise.withResolvers()
			export const pB = Promise.withResolvers()`,
		"a":  `import "./a-sentinel"; import "./b"`,
		"b":  `import "./b-sentinel"; import {p1} from "./setup"; await p1.promise`,
		"as": `import {pA} from "./setup"; pA.resolve()`,
		"bs": `import {pB} from "./setup"; pB.resolve()`,
	}
	alias := map[string]string{"./a-sentinel": "as", "./b-sentinel": "bs", "./b": "b", "./setup": "setup"}
	rt := quickjs.New()
	defer rt.Close()
	rt.SetModuleLoader(func(spec, referrer string) (string, string, error) {
		if a, ok := alias[spec]; ok {
			spec = a
		}
		src, ok := mods[spec]
		if !ok {
			return "", "", fmt.Errorf("unknown module %q", spec)
		}
		return src, spec, nil
	})
	_, err := rt.EvalModule("entry", `
		import {p1, pA, pB} from "./setup";
		globalThis.logs = []
		const importsP = Promise.all([
		  pB.promise.then(() => import("a").finally(() => logs.push("A"))).catch(function (e) { logs.push("Aerr") }),
		  import("b").finally(() => logs.push("B")).catch(function (e) { logs.push("Berr") }),
		])
		Promise.all([pA.promise, pB.promise]).then(p1.reject)
		importsP.then(() => { globalThis.done = logs.join(",") })`)
	t.Logf("err=%v", err)
	v, _ := rt.Get("done")
	l, _ := rt.Get("logs")
	t.Logf("done=%v logs=%v", v, l)
}
