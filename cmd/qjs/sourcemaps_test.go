package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnableSourceMaps runs a compiled program with --enable-source-maps,
// as node runs it: its map read from the file its comment names, beside
// it, the stack placed in the source it was compiled from, which is a path
// of the platform's own; and a copy of it under node_modules left as it
// is, as node leaves one.
func TestEnableSourceMaps(t *testing.T) {
	// qjs names a program by its real path, as node does: macOS's temporary
	// directory is under a link, /var to /private/var, and Windows's may be
	// named in the short form, RUNNER~1.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"src/app.ts":                       "function add(a: number, b: number): number {\n  if (b > 100) {\n    throw new Error(\"too big: \" + b);\n  }\n  return a + b;\n}\nclass Calc {\n  total = 0;\n  push(n: number) { this.total = add(this.total, n); }\n}\nconst c = new Calc();\nc.push(50);\nc.push(150);\n",
		"dist/app.js":                      "\"use strict\";\nvar __header = 1;\nfunction a(x, y) {\n    if (y > 100) {\n        throw new Error(\"too big: \" + y);\n    }\n    return x + y;\n}\nclass Calc {\n    constructor() { this.total = 0; }\n    push(n) { this.total = a(this.total, n); }\n}\nconst c = new Calc();\nc.push(50);\nc.push(150);\n" + "//# sourceMappingURL=app.js.map\n",
		"dist/app.js.map":                  "{\"version\": 3, \"file\": \"app.js\", \"sourceRoot\": \"\", \"sources\": [\"../src/app.ts\"], \"sourcesContent\": [\"function add(a: number, b: number): number {\\n  if (b > 100) {\\n    throw new Error(\\\"too big: \\\" + b);\\n  }\\n  return a + b;\\n}\\nclass Calc {\\n  total = 0;\\n  push(n: number) { this.total = add(this.total, n); }\\n}\\nconst c = new Calc();\\nc.push(50);\\nc.push(150);\\n\"], \"names\": [\"add\", \"push\"], \"mappings\": \";;AAAA,SAASA;IACP;QACE,MAAM;;IAER;;AAEF;;IAEEC,UAAkB,aAAaD;;AAEjC,UAAU;AACV,EAAEC;AACF,EAAEA;\"}",
		"node_modules/pkg/dist/app.js":     "\"use strict\";\nvar __header = 1;\nfunction a(x, y) {\n    if (y > 100) {\n        throw new Error(\"too big: \" + y);\n    }\n    return x + y;\n}\nclass Calc {\n    constructor() { this.total = 0; }\n    push(n) { this.total = a(this.total, n); }\n}\nconst c = new Calc();\nc.push(50);\nc.push(150);\n" + "//# sourceMappingURL=app.js.map\n",
		"node_modules/pkg/dist/app.js.map": "{\"version\": 3, \"file\": \"app.js\", \"sourceRoot\": \"\", \"sources\": [\"../src/app.ts\"], \"sourcesContent\": [\"function add(a: number, b: number): number {\\n  if (b > 100) {\\n    throw new Error(\\\"too big: \\\" + b);\\n  }\\n  return a + b;\\n}\\nclass Calc {\\n  total = 0;\\n  push(n: number) { this.total = add(this.total, n); }\\n}\\nconst c = new Calc();\\nc.push(50);\\nc.push(150);\\n\"], \"names\": [\"add\", \"push\"], \"mappings\": \";;AAAA,SAASA;IACP;QACE,MAAM;;IAER;;AAEF;;IAEEC,UAAkB,aAAaD;;AAEjC,UAAU;AACV,EAAEC;AACF,EAAEA;\"}",
	} {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	src := filepath.Join(dir, "src", "app.ts")
	_, _, errOut := exec(t, "", "--enable-source-maps", filepath.Join(dir, "dist", "app.js"))
	want := "Error: too big: 150\n    at add (" + src + ":3:11)\n    at Calc.push (" + src + ":9:34)\n    at Object.<anonymous> (" + src + ":13:3)"
	if !strings.Contains(errOut, want) {
		t.Errorf("mapped:\n%s\nwant:\n%s", errOut, want)
	}
	pkg := filepath.Join(dir, "node_modules", "pkg", "dist", "app.js")
	_, _, errOut = exec(t, "", "--enable-source-maps", pkg)
	if !strings.Contains(errOut, "    at a ("+pkg+":5:15)") {
		t.Errorf("node_modules:\n%s", errOut)
	}
}
