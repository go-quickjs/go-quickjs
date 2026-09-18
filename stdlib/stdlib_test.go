package stdlib_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
)

// run evaluates source in a runtime with the given configuration and returns
// what it produced, having let the loop finish.
func run(t *testing.T, cfg stdlib.Config, src string) (string, string) {
	t.Helper()
	rt := quickjs.New()
	defer rt.Close()

	var out, errOut bytes.Buffer
	cfg.Stdout, cfg.Stderr = &out, &errOut
	loop := stdlib.NewLoop(rt)
	if cfg.Loop == nil {
		cfg.Loop = loop
	}
	if err := stdlib.Install(rt, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(src); err != nil {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := cfg.Loop.Run(ctx); err != nil {
		t.Fatalf("loop: %v", err)
	}
	return strings.TrimRight(out.String(), "\n"), strings.TrimRight(errOut.String(), "\n")
}

func TestConsole(t *testing.T) {
	out, errOut := run(t, stdlib.Config{}, `
		console.log("plain")
		console.log("a", 1, true, null, undefined)
		console.log({a: 1, b: "two", c: [1, 2]})
		console.log([1, [2, [3, [4]]]])
		console.log(new Map([["k", 1]]), new Set([1, 2]))
		console.log(function named() {}, () => {}, class Thing {})
		console.log("%s is %d years", "Ada", 36.7)
		console.log("%o", {a: 1})
		console.error("to stderr")
		console.warn("also stderr")
		console.group("group")
		console.log("inside")
		console.groupEnd()
		console.log("outside")
		const cyclic = {name: "x"}
		cyclic.self = cyclic
		console.log(cyclic)
		console.log(new Date(0))
		console.log(/ab+c/gi)
		console.log(Symbol("s"), 10n)
		console.log(new Uint8Array([1, 2, 3]))
		console.count("c"); console.count("c")
	`)
	want := []string{
		"plain",
		"a 1 true null undefined",
		"{ a: 1, b: 'two', c: [ 1, 2 ] }",
		"[ 1, [ 2, [ 3, [ 4 ] ] ] ]",
		"Map(1) { 'k' => 1 } Set(2) { 1, 2 }",
		"[function: named] [function (anonymous)] [class: Thing]",
		"Ada is 36 years",
		"{ a: 1 }",
		"group",
		"  inside",
		"outside",
		"<ref> { name: 'x', self: [Circular] }",
		"1970-01-01T00:00:00.000Z",
		"/ab+c/gi",
		"Symbol(s) 10n",
		"Uint8Array(3) [ 1, 2, 3 ]",
		"c: 1",
		"c: 2",
	}
	lines := strings.Split(out, "\n")
	for i, w := range want {
		if i >= len(lines) {
			t.Fatalf("output ended after %d lines, want %q", len(lines), w)
		}
		// The cyclic line is the one place the exact wording is not pinned.
		if strings.Contains(w, "Circular") {
			if !strings.Contains(lines[i], "[Circular]") {
				t.Errorf("line %d = %q, want a circular marker", i, lines[i])
			}
			continue
		}
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
	if !strings.Contains(errOut, "to stderr") || !strings.Contains(errOut, "also stderr") {
		t.Errorf("stderr = %q", errOut)
	}
}

func TestTimers(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const order = []
		setTimeout(() => { order.push("t20"); done() }, 20)
		setTimeout(() => order.push("t0"), 0)
		queueMicrotask(() => order.push("micro"))
		Promise.resolve().then(() => order.push("promise"))
		order.push("sync")
		function done() { console.log(order.join(",")) }
	`)
	if want := "sync,micro,promise,t0,t20"; out != want {
		t.Errorf("order = %q, want %q", out, want)
	}
}

func TestIntervalAndClear(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		let n = 0
		const id = setInterval(() => {
			n++
			if (n === 3) { clearInterval(id); console.log("ran " + n) }
		}, 1)
		const never = setTimeout(() => console.log("never"), 5)
		clearTimeout(never)
	`)
	if want := "ran 3"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

// A timer that throws stops the loop and reports, rather than being swallowed.
func TestTimerErrorReaches(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`setTimeout(() => { throw new Error("from a timer") }, 0)`); err != nil {
		t.Fatal(err)
	}
	err := loop.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "from a timer") {
		t.Fatalf("loop error = %v, want the timer's", err)
	}
}

func TestPath(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		import("path").then(({default: path}) => {
			console.log(path.join("a", "b", "../c"))
			console.log(path.basename("/a/b/c.txt"), path.basename("/a/b/c.txt", ".txt"))
			console.log(path.dirname("/a/b/c.txt"), path.extname("x.tar.gz"))
			console.log(path.isAbsolute("/a"), path.isAbsolute("a"))
			console.log(path.normalize("/a//b/../c/"))
			console.log(path.relative("/a/b", "/a/c/d"))
			console.log(path.resolve("/a", "b", "../c"))
		})
	`)
	want := strings.Join([]string{
		"a/c",
		"c.txt c",
		"/a/b .gz",
		"true false",
		"/a/c/",
		"../c/d",
		"/a/c",
	}, "\n")
	if out != want {
		t.Errorf("path output =\n%s\nwant\n%s", out, want)
	}
}

func TestFilesSync(t *testing.T) {
	dir := t.TempDir()
	out, _ := run(t, stdlib.Config{FS: &stdlib.FS{Root: dir}}, `
		import("fs").then(({default: fs}) => {
			fs.writeFileSync("/hello.txt", "hello")
			console.log(fs.readFileSync("/hello.txt", "utf8"))
			console.log(fs.existsSync("/hello.txt"), fs.existsSync("/nope.txt"))
			fs.appendFileSync("/hello.txt", " world")
			console.log(fs.readFileSync("/hello.txt", "utf8"))

			const bytes = fs.readFileSync("/hello.txt")
			console.log(bytes.constructor.name, bytes.length)

			fs.mkdirSync("/sub/deep", {recursive: true})
			fs.writeFileSync("/sub/deep/a.txt", "a")
			console.log(fs.readdirSync("/sub"))
			const st = fs.statSync("/sub")
			console.log(st.isDirectory(), st.isFile())
			console.log(fs.statSync("/hello.txt").size)

			fs.renameSync("/hello.txt", "/renamed.txt")
			console.log(fs.existsSync("/renamed.txt"))
			fs.copyFileSync("/renamed.txt", "/copy.txt")
			fs.rmSync("/copy.txt")
			console.log(fs.existsSync("/copy.txt"))
			fs.rmSync("/sub", {recursive: true})
			console.log(fs.readdirSync("/"))
		})
	`)
	want := strings.Join([]string{
		"hello",
		"true false",
		"hello world",
		"Uint8Array 11",
		"[ 'deep' ]",
		"true false",
		"11",
		"true",
		"false",
		"[ 'renamed.txt' ]",
	}, "\n")
	if out != want {
		t.Errorf("fs output =\n%s\nwant\n%s", out, want)
	}
}

func TestFilesPromises(t *testing.T) {
	dir := t.TempDir()
	out, _ := run(t, stdlib.Config{FS: &stdlib.FS{Root: dir}}, `
		;(async () => {
			const fs = (await import("fs/promises")).default
			await fs.writeFile("/a.txt", "from a promise")
			console.log(await fs.readFile("/a.txt", "utf8"))
			const st = await fs.stat("/a.txt")
			console.log(st.size, st.isFile())
			console.log(await fs.readdir("/"))
			await fs.rm("/a.txt")
			console.log((await fs.readdir("/")).length)
		})()
	`)
	want := "from a promise\n14 true\n[ 'a.txt' ]\n0"
	if out != want {
		t.Errorf("fs.promises output =\n%s\nwant\n%s", out, want)
	}
}

// A root is a boundary: a path that tries to leave it is refused, however it is
// spelled, and a link that points outside it does not lead outside it.
func TestFilesRootConfines(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	inside := filepath.Join(dir, "inside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(inside, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, _ := run(t, stdlib.Config{FS: &stdlib.FS{Root: inside}}, `
		import("fs").then(({default: fs}) => {
			for (const p of ["../outside/secret.txt", "/../outside/secret.txt",
			                 "/a/../../outside/secret.txt", "link.txt"]) {
				try { fs.readFileSync(p, "utf8"); console.log("READ " + p) }
				catch (e) { console.log("refused") }
			}
		})
	`)
	if want := "refused\nrefused\nrefused\nrefused"; out != want {
		t.Errorf("confinement =\n%s\nwant\n%s", out, want)
	}
}

func TestFilesReadOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r.txt"), []byte("readable"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := run(t, stdlib.Config{FS: &stdlib.FS{Root: dir, ReadOnly: true}}, `
		import("fs").then(({default: fs}) => {
			console.log(fs.readFileSync("/r.txt", "utf8"))
			try { fs.writeFileSync("/w.txt", "no"); console.log("wrote") }
			catch (e) { console.log("refused: " + e.message) }
		})
	`)
	if !strings.HasPrefix(out, "readable\nrefused: ") {
		t.Errorf("read-only output = %q", out)
	}
}

func TestProcess(t *testing.T) {
	out, _ := run(t, stdlib.Config{Process: &stdlib.Process{
		Args: []string{"qjs", "script.js", "--flag"},
		Env:  map[string]string{"HOME": "/home/x", "LANG": "C"},
		Cwd:  "/work",
	}}, `
		console.log(process.argv.slice(2).join())
		console.log(process.env.HOME, process.env.LANG, process.env.MISSING)
		console.log(process.cwd())
		console.log(typeof process.platform, typeof process.arch)
		process.stdout.write("written\n")
		const [s, ns] = process.hrtime()
		console.log(typeof s, typeof ns)
		try { process.exit(1) } catch (e) { console.log("cannot exit") }
	`)
	want := strings.Join([]string{
		"--flag",
		"/home/x C undefined",
		"/work",
		"string string",
		"written",
		"number number",
		"cannot exit",
	}, "\n")
	if out != want {
		t.Errorf("process output =\n%s\nwant\n%s", out, want)
	}
}

func TestOS(t *testing.T) {
	out, _ := run(t, stdlib.Config{OS: &stdlib.OSInfo{Hostname: "given"}}, `
		import("os").then(({default: os}) => {
			console.log(os.hostname(), os.homedir() === "", os.cpus() > 0)
			console.log(typeof os.platform(), os.EOL === "\n")
		})
	`)
	want := "given true true\nstring true"
	if out != want {
		t.Errorf("os output =\n%s\nwant\n%s", out, want)
	}
}

func TestURL(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const u = new URL("https://user:pw@example.com:8443/a/b/../c?x=1&y=2#frag")
		console.log(u.protocol, u.hostname, u.port, u.pathname, u.search, u.hash)
		console.log(u.username, u.password, u.host, u.origin)
		console.log(u.href)
		console.log(String(new URL("https://example.com:443/x")))
		console.log(new URL("d", "https://example.com/a/b/c").href)
		console.log(new URL("/d", "https://example.com/a/b/c").href)
		console.log(new URL("?q=1", "https://example.com/a").href)
		console.log(new URL("#h", "https://example.com/a").href)
		const p = new URL("https://x.test/?a=1&a=2&b=3").searchParams
		console.log(p.getAll("a").join("|"), p.get("b"), p.has("c"))
		p.append("c", "hello world")
		console.log(p.toString())
		const withParams = new URL("https://x.test/p")
		withParams.searchParams.set("q", "a&b")
		console.log(withParams.href)
		console.log([...new URLSearchParams("a=1&b=2")].map(e => e.join(":")).join(","))
		try { new URL("not a url") } catch (e) { console.log(e.constructor.name) }
	`)
	want := strings.Join([]string{
		"https: example.com 8443 /a/c ?x=1&y=2 #frag",
		"user pw example.com:8443 https://example.com:8443",
		"https://user:pw@example.com:8443/a/c?x=1&y=2#frag",
		"https://example.com/x",
		"https://example.com/a/b/d",
		"https://example.com/d",
		"https://example.com/a?q=1",
		"https://example.com/a#h",
		"1|2 3 false",
		"a=1&a=2&b=3&c=hello+world",
		"https://x.test/p?q=a%26b",
		"a:1,b:2",
		"TypeError",
	}, "\n")
	if out != want {
		t.Errorf("URL output =\n%s\nwant\n%s", out, want)
	}
}

func TestTextCodec(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const enc = new TextEncoder(), dec = new TextDecoder()
		console.log(Array.from(enc.encode("aé😀")).join())
		console.log(dec.decode(enc.encode("aé😀")))
		console.log(enc.encoding, dec.encoding)
		// An unpaired surrogate is not valid UTF-8, so it is replaced.
		console.log(Array.from(enc.encode("\uD800")).join())
		// Bad input decodes to the replacement character, or throws when fatal.
		console.log(dec.decode(new Uint8Array([0xff, 0x41])))
		try { new TextDecoder("utf-8", {fatal: true}).decode(new Uint8Array([0xff])) }
		catch (e) { console.log("fatal: " + e.constructor.name) }
		console.log(dec.decode(new Uint8Array([0xef, 0xbb, 0xbf, 0x41])))
	`)
	want := strings.Join([]string{
		"97,195,169,240,159,152,128",
		"aé😀",
		"utf-8 utf-8",
		"239,191,189",
		"�A",
		"fatal: TypeError",
		"A",
	}, "\n")
	if out != want {
		t.Errorf("text codec output =\n%s\nwant\n%s", out, want)
	}
}

func TestCryptoAndClone(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			const uuid = crypto.randomUUID()
			console.log(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(uuid))
			const a = crypto.getRandomValues(new Uint8Array(8))
			const b = crypto.getRandomValues(new Uint8Array(8))
			console.log(a.length, a.join() !== b.join())
			const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode("abc"))
			console.log(Array.from(new Uint8Array(digest)).slice(0, 4)
				.map(x => x.toString(16).padStart(2, "0")).join(""))

			const original = {n: 1, list: [1, 2], when: new Date(0), re: /x/g,
			                  map: new Map([["k", "v"]]), bytes: new Uint8Array([1, 2])}
			original.self = original
			const copy = structuredClone(original)
			console.log(copy.self === copy, copy.n, copy.list.join(), copy.when.getTime())
			console.log(copy.re.source, copy.map.get("k"), copy.bytes[1])
			console.log(copy.list !== original.list, copy.map !== original.map)
			try { structuredClone(() => {}) } catch (e) { console.log("no functions") }
		})()
	`)
	want := strings.Join([]string{
		"true",
		"8 true",
		"ba7816bf",
		"true 1 1,2 0",
		"x v 2",
		"true true",
		"no functions",
	}, "\n")
	if out != want {
		t.Errorf("crypto output =\n%s\nwant\n%s", out, want)
	}
}

func TestBase64(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		console.log(btoa("hello"), atob("aGVsbG8="))
		console.log(atob("aGVsbG8"))
		// Latin-1 is fine -- btoa works on bytes, and é is one.
		console.log(btoa("é"))
		try { btoa("中") } catch (e) { console.log("out of range") }
		try { atob("!!!") } catch (e) { console.log("bad input") }
	`)
	want := "aGVsbG8= hello\nhello\n6Q==\nout of range\nbad input"
	if out != want {
		t.Errorf("base64 output =\n%s\nwant\n%s", out, want)
	}
}

func TestAbortController(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const c = new AbortController()
		console.log(c.signal.aborted)
		c.signal.addEventListener("abort", () => console.log("aborted:", c.signal.reason.message))
		c.abort(new Error("because"))
		console.log(c.signal.aborted)
		c.abort(new Error("again"))
		console.log(AbortSignal.abort().aborted)
	`)
	want := "false\naborted: because\ntrue\ntrue"
	if out != want {
		t.Errorf("abort output =\n%s\nwant\n%s", out, want)
	}
}

// Nothing is installed that was not asked for: a runtime given no filesystem
// has no way to reach one.
func TestNothingIsAmbient(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	if err := stdlib.Install(rt, stdlib.Config{}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		`typeof process`,
		`typeof fetch`,
		`typeof setTimeout`,
	} {
		v, err := rt.Eval(src)
		if err != nil {
			t.Fatal(err)
		}
		if v.String() != "undefined" {
			t.Errorf("%s = %q, want undefined", src, v.String())
		}
	}
	if _, err := rt.EvalModule("main.js", `import "fs"`); err == nil {
		t.Error("importing fs should have failed")
	}
}
