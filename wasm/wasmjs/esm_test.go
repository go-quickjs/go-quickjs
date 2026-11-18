//go:build quickjs_wasm

package wasmjs_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"github.com/go-quickjs/go-quickjs/wasm/wasmjs"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// The ES module integration's tests: each archive of testdata/esm is a tree
// of modules -- .wat files compiled to the .wasm files beside them, and .bin
// files of bytes -- whose entries, the files named t<digits>.mjs, are
// imported one after the other by a runner, which records whether each
// import settled and what the entry recorded, as lines; "want" is the
// archive's expected output, which QUICKJS_ESM_UPDATE=1 rewrites from what
// go-quickjs records. Paths in the output read $DIR for the tree's directory
// and $URL for its file: URL.

// esmHarness is the module the entries record with: p records what a call
// returns or throws, pa what a promise settles with.
const esmHarness = `export const out = [];
let chain = Promise.resolve();
export function fmt(r) {
  if (typeof r === "bigint") return r + "n";
  if (typeof r === "string") return JSON.stringify(r);
  if (typeof r === "function") return "function " + r.name + "/" + r.length;
  if (typeof r === "object" && r !== null) {
    if (Array.isArray(r)) return "[" + r.map(fmt).join(", ") + "]";
    return Object.prototype.toString.call(r);
  }
  if (Object.is(r, -0)) return "-0";
  return String(r);
}
export function show(e) {
  if (e !== null && typeof e === "object" && "message" in e) {
    return (e.constructor && e.constructor.name) + ": " + e.message + (e.code ? " [" + e.code + "]" : "");
  }
  return "threw " + fmt(e);
}
export function p(label, f) {
  try {
    out.push(label + " -> " + fmt(f()));
  } catch (e) {
    out.push(label + " -> " + show(e));
  }
}
export function pa(label, f) {
  chain = chain.then(async () => {
    try {
      out.push(label + " -> " + fmt(await f()));
    } catch (e) {
      out.push(label + " -> " + show(e));
    }
  });
}
export function settled() { return chain; }
`

// esmRunner is the runner of the entries given.
func esmRunner(entries []string) string {
	var b strings.Builder
	b.WriteString("import { out, show, settled } from \"./harness.mjs\";\n")
	for _, e := range entries {
		b.WriteString("try { await import(\"./" + e + "\"); out.push(\"" + e + " -> ok\"); } catch (e) { out.push(\"" + e + " -> \" + show(e)); }\n")
		b.WriteString("await settled();\n")
	}
	b.WriteString("globalThis.result = out.join(\"\\n\") + \"\\n\";\n")
	return b.String()
}

// esmArchive is an archive's files, by name, and the entries among them.
type esmArchive struct {
	files   map[string]string
	entries []string
	want    string
}

// readESMArchive reads an archive: sections each headed "-- name --".
func readESMArchive(t *testing.T, path string) esmArchive {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a := esmArchive{files: map[string]string{}}
	name := ""
	for _, line := range strings.SplitAfter(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if h, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "-- "); ok && strings.HasSuffix(h, " --") {
			name = strings.TrimSuffix(h, " --")
			a.files[name] = ""
			continue
		}
		if name != "" {
			a.files[name] += line
		}
	}
	a.want = a.files["want"]
	delete(a.files, "want")
	for name := range a.files {
		if isESMEntry(name) {
			a.entries = append(a.entries, name)
		}
	}
	sort.Strings(a.entries)
	return a
}

// isESMEntry reports whether a file of an archive is an entry: t, digits,
// .mjs.
func isESMEntry(name string) bool {
	digits, ok := strings.CutPrefix(strings.TrimSuffix(name, ".mjs"), "t")
	if !ok || digits == "" || !strings.HasSuffix(name, ".mjs") {
		return false
	}
	return strings.Trim(digits, "0123456789") == ""
}

// writeESMArchive writes an archive's tree, with the harness and the runner,
// to a new directory, and returns it.
func writeESMArchive(t *testing.T, a esmArchive) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"harness.mjs": esmHarness, "runner.mjs": esmRunner(a.entries)}
	for name, src := range a.files {
		files[name] = src
	}
	for name, src := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		data := []byte(src)
		if strings.HasSuffix(name, ".wat") {
			if data, err = wat.Compile(data); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			p = strings.TrimSuffix(p, ".wat") + ".wasm"
		}
		if strings.HasSuffix(name, ".bin") {
			// Bytes written as text: \xNN escapes.
			data = unescapeBytes(src)
			p = strings.TrimSuffix(p, ".bin")
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// unescapeBytes reads text whose \xNN escapes are bytes, ignoring its
// newlines.
func unescapeBytes(s string) []byte {
	s = strings.ReplaceAll(s, "\n", "")
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			var v byte
			for _, c := range s[i+2 : i+4] {
				v <<= 4
				switch {
				case c >= '0' && c <= '9':
					v |= byte(c - '0')
				case c >= 'a' && c <= 'f':
					v |= byte(c - 'a' + 10)
				}
			}
			out = append(out, v)
			i += 3
			continue
		}
		out = append(out, s[i])
	}
	return out
}

// fileURLOf is the file: URL of an absolute path, as Node writes one.
func fileURLOf(p string) string {
	s := filepath.ToSlash(p)
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return "file://" + s
}

// normalizeESM puts $DIR and $URL for the tree's directory in out.
func normalizeESM(out, dir string) string {
	out = strings.ReplaceAll(out, fileURLOf(dir), "$URL")
	out = strings.ReplaceAll(out, strings.ReplaceAll(dir, `\`, `\\`), "$DIR")
	out = strings.ReplaceAll(out, dir, "$DIR")
	// The rest of a path is written with slashes on every system.
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, "$DIR") {
			lines[i] = strings.ReplaceAll(strings.ReplaceAll(l, `\\`, "/"), `\`, "/")
		}
	}
	return strings.Join(lines, "\n")
}

// runESMQuickJS runs an archive's tree in a runtime with Node's quirks, the
// standard library and the WebAssembly namespace, whose loader reads the
// tree's files as Node's does -- a specifier resolved against the
// importer's directory, a module named by its path -- and returns what the
// runner recorded. With async the loader is an AsyncModuleLoader, which
// answers from a goroutine of its own.
func runESMQuickJS(t *testing.T, dir string, async ...bool) string {
	t.Helper()
	rt := quickjs.New(quickjs.WithNodeQuirks())
	defer rt.Close()
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	if err := wasmjs.Install(rt, wasmjs.WithModuleURL(fileURLOf)); err != nil {
		t.Fatal(err)
	}
	load := func(specifier, referrer string) (string, string, error) {
		from := dir
		if referrer != "" {
			from = filepath.Dir(referrer)
		}
		p := filepath.Join(from, filepath.FromSlash(specifier))
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			e := rt.NewError("Error", "Cannot find module '"+p+"' imported from "+referrer)
			e.Set("code", "ERR_MODULE_NOT_FOUND")
			return "", "", rt.Throw(e)
		}
		return string(b), p, err
	}
	if len(async) > 0 && async[0] {
		rt.SetAsyncModuleLoader(func(ctx context.Context, req quickjs.ModuleRequest, done func(quickjs.LoadedModule, error)) {
			source, resolved, err := load(req.Specifier, req.Referrer)
			go done(quickjs.LoadedModule{Source: source, Resolved: resolved}, err)
		})
	} else {
		rt.SetModuleLoader(load)
	}
	runner := filepath.Join(dir, "runner.mjs")
	src, err := os.ReadFile(runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.EvalModule(runner, string(src)); err != nil {
		t.Fatal(err)
	}
	if err := loop.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	v, err := rt.Eval(`globalThis.result ?? "(the runner did not finish)\n"`)
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

// TestESMIntegration runs each archive of testdata/esm and compares what it
// records with its want section.
func TestESMIntegration(t *testing.T) {
	archives, err := filepath.Glob(filepath.Join("testdata", "esm", "*.txt"))
	if err != nil || len(archives) == 0 {
		t.Fatal("no archives", err)
	}
	for _, path := range archives {
		t.Run(filepath.Base(path), func(t *testing.T) {
			a := readESMArchive(t, path)
			dir := writeESMArchive(t, a)
			got := normalizeESM(runESMQuickJS(t, dir), dir)
			// An asynchronous loader's graph is the same graph.
			if async := normalizeESM(runESMQuickJS(t, dir, true), dir); async != got {
				t.Errorf("with an asynchronous loader:\n%s\nwith a loader:\n%s", async, got)
			}
			if os.Getenv("QUICKJS_ESM_UPDATE") == "1" {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				head, _, _ := strings.Cut(strings.ReplaceAll(string(b), "\r\n", "\n"), "-- want --\n")
				if err := os.WriteFile(path, []byte(head+"-- want --\n"+got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			if got != a.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, a.want)
			}
		})
	}
}

// esmNodeKnownDifferences are the lines, by archive and label, where
// go-quickjs is known to differ from Node, and why. None is the module
// integration's own: each is a message the runtime words its own way.
var esmNodeKnownDifferences = map[string]string{
	"errors.txt: t03.mjs":    "the runtime's own message for an export a module does not have",
	"errors.txt: t13.mjs":    "the runtime's own message for a binding read before it is initialized",
	"errors.txt: t14.mjs":    "the runtime's own message for a binding read before it is initialized",
	"errors.txt: t15.mjs":    "the runtime's own message for the source of a JavaScript module, which Node's loader words",
	"errors.txt: t16.mjs":    "the runtime's own message for the source of a JavaScript module, which Node's loader words",
	"basic.txt: namespace =": "the runtime's own message for an assignment to a namespace's export",
}

// TestCompareNodeESM runs each archive of testdata/esm in Node and in
// go-quickjs and compares what they record, line by line.
func TestCompareNodeESM(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") != "1" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with Node")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node on PATH")
	}
	archives, err := filepath.Glob(filepath.Join("testdata", "esm", "*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range archives {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			a := readESMArchive(t, path)
			dir := writeESMArchive(t, a)
			cmd := exec.Command(node, "--no-warnings", "--input-type=module", "-e",
				`await import("./runner.mjs"); process.stdout.write(globalThis.result)`)
			cmd.Dir = dir
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("node: %v", err)
			}
			want := normalizeESM(string(out), dir)
			got := normalizeESM(runESMQuickJS(t, dir), dir)
			w := strings.Split(strings.TrimRight(want, "\n"), "\n")
			g := strings.Split(strings.TrimRight(got, "\n"), "\n")
			for i := 0; i < max(len(w), len(g)); i++ {
				var wl, gl string
				if i < len(w) {
					wl = w[i]
				}
				if i < len(g) {
					gl = g[i]
				}
				if wl == gl {
					continue
				}
				label, _, _ := strings.Cut(wl, " -> ")
				if why := esmNodeKnownDifferences[name+": "+label]; why != "" {
					t.Logf("line %d differs, as known (%s):\n node:       %s\n go-quickjs: %s", i+1, why, wl, gl)
					continue
				}
				t.Errorf("line %d:\n node:       %s\n go-quickjs: %s", i+1, wl, gl)
			}
		})
	}
}
