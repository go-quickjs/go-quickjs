package quickjs_test

import (
	"errors"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// The program TestSourceMaps runs: a TypeScript-like source compiled with
// its lines moved and add renamed a, and its map, inline and on its own.
// Node v26.10 with --enable-source-maps writes its stack as want does.
const (
	mappedInline = "\"use strict\";\nvar __header = 1;\nfunction a(x, y) {\n    if (y > 100) {\n        throw new Error(\"too big: \" + y);\n    }\n    return x + y;\n}\nclass Calc {\n    constructor() { this.total = 0; }\n    push(n) { this.total = a(this.total, n); }\n}\nconst c = new Calc();\nc.push(50);\nc.push(150);\n\n//# sourceMappingURL=data:application/json;charset=utf-8;base64,eyJ2ZXJzaW9uIjogMywgImZpbGUiOiAiYXBwLmpzIiwgInNvdXJjZVJvb3QiOiAiIiwgInNvdXJjZXMiOiBbIi4uL3NyYy9hcHAudHMiXSwgInNvdXJjZXNDb250ZW50IjogWyJmdW5jdGlvbiBhZGQoYTogbnVtYmVyLCBiOiBudW1iZXIpOiBudW1iZXIge1xuICBpZiAoYiA+IDEwMCkge1xuICAgIHRocm93IG5ldyBFcnJvcihcInRvbyBiaWc6IFwiICsgYik7XG4gIH1cbiAgcmV0dXJuIGEgKyBiO1xufVxuY2xhc3MgQ2FsYyB7XG4gIHRvdGFsID0gMDtcbiAgcHVzaChuOiBudW1iZXIpIHsgdGhpcy50b3RhbCA9IGFkZCh0aGlzLnRvdGFsLCBuKTsgfVxufVxuY29uc3QgYyA9IG5ldyBDYWxjKCk7XG5jLnB1c2goNTApO1xuYy5wdXNoKDE1MCk7XG4iXSwgIm5hbWVzIjogWyJhZGQiLCAicHVzaCJdLCAibWFwcGluZ3MiOiAiOztBQUFBLFNBQVNBO0lBQ1A7UUFDRSxNQUFNOztJQUVSOztBQUVGOztJQUVFQyxVQUFrQixhQUFhRDs7QUFFakMsVUFBVTtBQUNWLEVBQUVDO0FBQ0YsRUFBRUE7In0=\n"
	mappedPlain  = "\"use strict\";\nvar __header = 1;\nfunction a(x, y) {\n    if (y > 100) {\n        throw new Error(\"too big: \" + y);\n    }\n    return x + y;\n}\nclass Calc {\n    constructor() { this.total = 0; }\n    push(n) { this.total = a(this.total, n); }\n}\nconst c = new Calc();\nc.push(50);\nc.push(150);\n"
	mappedMap    = "{\"version\": 3, \"file\": \"app.js\", \"sourceRoot\": \"\", \"sources\": [\"../src/app.ts\"], \"sourcesContent\": [\"function add(a: number, b: number): number {\\n  if (b > 100) {\\n    throw new Error(\\\"too big: \\\" + b);\\n  }\\n  return a + b;\\n}\\nclass Calc {\\n  total = 0;\\n  push(n: number) { this.total = add(this.total, n); }\\n}\\nconst c = new Calc();\\nc.push(50);\\nc.push(150);\\n\"], \"names\": [\"add\", \"push\"], \"mappings\": \";;AAAA,SAASA;IACP;QACE,MAAM;;IAER;;AAEF;;IAEEC,UAAkB,aAAaD;;AAEjC,UAAU;AACV,EAAEC;AACF,EAAEA;\"}"
)

// The stack, as Node writes it: each frame in the source, named as the map
// names it -- a, which add was compiled to, add again.
const mappedWant = "Error: too big: 150\n    at add (/proj/src/app.ts:3:11)\n    at Calc.push (/proj/src/app.ts:9:34)\n    at <anonymous> (/proj/src/app.ts:13:3)"

// stackOf runs the program in a runtime made with opts and returns the
// stack of what it threw.
func stackOf(t *testing.T, src string, opts ...quickjs.Option) string {
	t.Helper()
	rt := quickjs.New(opts...)
	defer rt.Close()
	_, err := rt.EvalFile("/proj/dist/app.js", src)
	var e *quickjs.Error
	if !errors.As(err, &e) {
		t.Fatalf("no error thrown: %v", err)
	}
	stack, err := e.Value().Get("stack")
	if err != nil {
		t.Fatal(err)
	}
	return stack.String()
}

// TestSourceMaps pins stacks through source maps, as Node's
// --enable-source-maps writes them: through a map inline in the script,
// read with no loader; through one a loader reads, against whose URL its
// sources are resolved; and as they are without the option, with a loader
// that finds nothing, or through a script's Error.prepareStackTrace.
func TestSourceMaps(t *testing.T) {
	if got := stackOf(t, mappedInline, quickjs.WithSourceMaps(nil)); got != mappedWant {
		t.Errorf("inline:\n%s\nwant:\n%s", got, mappedWant)
	}

	var asked []string
	load := func(script, url string) ([]byte, string, error) {
		asked = append(asked, script+" "+url)
		return []byte(mappedMap), "/proj/dist/app.js.map", nil
	}
	file := mappedPlain + "\n//# sourceMappingURL=app.js.map\n"
	if got := stackOf(t, file, quickjs.WithSourceMaps(load)); got != mappedWant {
		t.Errorf("loaded:\n%s\nwant:\n%s", got, mappedWant)
	}
	if strings.Join(asked, "|") != "/proj/dist/app.js app.js.map" {
		t.Errorf("the loader was asked %q, once", asked)
	}

	unmapped := "Error: too big: 150\n    at a (/proj/dist/app.js:5:15)"
	for name, opts := range map[string][]quickjs.Option{
		"without the option": nil,
		"a loader finding nothing": {quickjs.WithSourceMaps(func(string, string) ([]byte, string, error) {
			return nil, "", errors.New("no map")
		})},
	} {
		if got := stackOf(t, file, opts...); !strings.HasPrefix(got, unmapped) {
			t.Errorf("%s:\n%s", name, got)
		}
	}

	prepared := "Error.prepareStackTrace = (e, cs) => cs[0].getFileName() + ':' + cs[0].getLineNumber();\n" + mappedInline
	if got := stackOf(t, prepared, quickjs.WithSourceMaps(nil)); got != "/proj/dist/app.js:6" {
		t.Errorf("prepareStackTrace: %q", got)
	}
}
