package stdlib_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestFetch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kind", "text")
		io.WriteString(w, "hello from the server")
	})
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"n":1,"list":[1,2]}`)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Method", r.Method)
		w.Header().Set("X-Custom", r.Header.Get("X-Custom"))
		w.Write(body)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	out, _ := run(t, stdlib.Config{Fetch: &stdlib.Fetch{}}, `
		;(async () => {
			const base = "`+srv.URL+`"
			const res = await fetch(base + "/text")
			console.log(res.status, res.ok, res.headers.get("x-kind"))
			console.log(await res.text())
			console.log(res.bodyUsed)
			try { await res.text() } catch (e) { console.log("read once") }

			const data = await (await fetch(base + "/json")).json()
			console.log(data.n, data.list.join())

			const echoed = await fetch(base + "/echo", {
				method: "POST",
				headers: {"X-Custom": "sent"},
				body: "the body",
			})
			console.log(echoed.headers.get("x-method"), echoed.headers.get("x-custom"))
			console.log(await echoed.text())

			const missing = await fetch(base + "/missing")
			console.log(missing.status, missing.ok)

			try { await fetch("http://127.0.0.1:1/nothing") }
			catch (e) { console.log("failed to connect") }
		})()
	`)
	want := strings.Join([]string{
		"200 true text",
		"hello from the server",
		"true",
		"read once",
		"1 1,2",
		"POST sent",
		"the body",
		"404 false",
		"failed to connect",
	}, "\n")
	if out != want {
		t.Errorf("fetch output =\n%s\nwant\n%s", out, want)
	}
}

// A host that says where a script may go is obeyed before the request is made.
func TestFetchAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "reached")
	}))
	defer srv.Close()

	out, _ := run(t, stdlib.Config{Fetch: &stdlib.Fetch{
		Allow: func(req *http.Request) error {
			if req.URL.Path != "/allowed" {
				return errors.New("that host is not allowed")
			}
			return nil
		},
	}}, `
		;(async () => {
			console.log(await (await fetch("`+srv.URL+`/allowed")).text())
			try { await fetch("`+srv.URL+`/other") }
			catch (e) { console.log("refused: " + /not allowed/.test(e.message)) }
		})()
	`)
	if want := "reached\nrefused: true"; out != want {
		t.Errorf("allow output =\n%s\nwant\n%s", out, want)
	}
}

// The request and response objects work on their own, which is what code that
// builds a request before sending it needs.
func TestFetchTypes(t *testing.T) {
	out, _ := run(t, stdlib.Config{Fetch: &stdlib.Fetch{}}, `
		;(async () => {
			const h = new Headers({"Content-Type": "text/plain"})
			h.append("x-a", "1")
			h.append("x-a", "2")
			console.log(h.get("x-a"), h.get("content-type"), h.has("x-b"))
			h.set("x-a", "3")
			console.log(h.get("x-a"), [...h.keys()].join())

			const req = new Request("https://example.com/p", {method: "post", body: "hi"})
			console.log(req.method, req.url, req.headers.get("content-type"))
			console.log(await req.text())

			const res = Response.json({ok: true}, {status: 201})
			console.log(res.status, res.headers.get("content-type"))
			console.log(JSON.stringify(await res.json()))

			try { new Request("https://x.test", {method: "GET", body: "no"}) }
			catch (e) { console.log("no body on GET") }
		})()
	`)
	want := strings.Join([]string{
		"1, 2 text/plain false",
		"3 content-type,x-a",
		"POST https://example.com/p text/plain;charset=UTF-8",
		"hi",
		"201 application/json",
		`{"ok":true}`,
		"no body on GET",
	}, "\n")
	if out != want {
		t.Errorf("types output =\n%s\nwant\n%s", out, want)
	}
}

// An abort stops the request rather than merely ignoring the answer.
func TestFetchAbort(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var cancelled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			cancelled.Store(true)
		case <-release:
			io.WriteString(w, "too late")
		}
	}))
	defer srv.Close()
	defer close(release)

	rt := quickjs.New()
	defer rt.Close()
	var out bytes.Buffer
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{
		Stdout: &out, Loop: loop, Fetch: &stdlib.Fetch{},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`
		const c = new AbortController()
		globalThis.abortIt = () => c.abort()
		fetch("` + srv.URL + `/slow", {signal: c.signal})
			.then(() => console.log("completed"))
			.catch(e => console.log("aborted"))
	`); err != nil {
		t.Fatal(err)
	}
	<-started
	if _, err := rt.Eval(`abortIt()`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "aborted" {
		t.Errorf("out = %q, want %q", got, "aborted")
	}
	// The server saw the request go away, which is the difference between
	// abandoning an answer and cancelling a request.
	deadline := time.Now().Add(2 * time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Error("the server did not see the request cancelled")
	}
}

func TestEvents(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			const {EventEmitter, once} = await import("events")
			const e = new EventEmitter()
			e.on("greet", name => console.log("hello", name))
			e.emit("greet", "world")
			console.log(e.listenerCount("greet"), e.eventNames().join())

			let n = 0
			e.once("tick", () => n++)
			e.emit("tick"); e.emit("tick")
			console.log("once fired", n)

			const fn = () => console.log("never")
			e.on("gone", fn)
			e.off("gone", fn)
			console.log(e.emit("gone"))

			// An error with nobody listening is thrown rather than swallowed.
			try { e.emit("error", new Error("unheard")) } catch (err) { console.log(err.message) }

			const waiter = once(e, "later")
			e.emit("later", 1, 2)
			console.log((await waiter).join())
		})()
	`)
	want := strings.Join([]string{
		"hello world",
		"1 greet",
		"once fired 1",
		"false",
		"unheard",
		"1,2",
	}, "\n")
	if out != want {
		t.Errorf("events output =\n%s\nwant\n%s", out, want)
	}
}

func TestUtil(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			const util = (await import("util")).default
			console.log(util.format("%s has %d and %j", "x", 3.7, {a: 1}))
			console.log(util.inspect({a: [1, "two"]}))
			const doubled = util.promisify((n, cb) => cb(null, n * 2))
			console.log(await doubled(21))
			const failing = util.promisify((cb) => cb(new Error("no good")))
			try { await failing() } catch (e) { console.log(e.message) }
			console.log(util.types.isDate(new Date()), util.types.isRegExp(/x/),
			            util.types.isTypedArray(new Uint8Array(1)))
			console.log(util.isDeepStrictEqual([1, {a: 2}], [1, {a: 2}]),
			            util.isDeepStrictEqual([1], [2]))
		})()
	`)
	want := strings.Join([]string{
		`x has 3 and {"a":1}`,
		"{ a: [ 1, 'two' ] }",
		"42",
		"no good",
		"true true true",
		"true false",
	}, "\n")
	if out != want {
		t.Errorf("util output =\n%s\nwant\n%s", out, want)
	}
}

func TestAssert(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			const assert = (await import("assert")).default
			assert.ok(true)
			assert.equal(1, "1")
			assert.strictEqual("a", "a")
			assert.notStrictEqual(1, 2)
			assert.deepStrictEqual({a: [1, {b: 2}]}, {a: [1, {b: 2}]})
			assert.deepStrictEqual(new Map([["k", 1]]), new Map([["k", 1]]))
			assert.deepStrictEqual(new Set([1]), new Set([1]))
			assert.match("abc", /b/)
			assert.throws(() => { throw new TypeError("x") }, TypeError)
			assert.doesNotThrow(() => 1)
			await assert.rejects(Promise.reject(new Error("no")))
			console.log("all passed")

			for (const [name, fn] of [
				["ok", () => assert.ok(false)],
				["strictEqual", () => assert.strictEqual(1, 2)],
				["deepStrictEqual", () => assert.deepStrictEqual({a: 1}, {a: 2})],
				["strict types", () => assert.deepStrictEqual(1, "1")],
				["match", () => assert.match("abc", /z/)],
				["throws", () => assert.throws(() => 1)],
			]) {
				try { fn(); console.log("MISSED " + name) }
				catch (e) { console.log(name + ": " + e.name) }
			}
			try { assert.strictEqual(1, 2, "a message of my own") }
			catch (e) { console.log(e.message) }
		})()
	`)
	want := strings.Join([]string{
		"all passed",
		"ok: AssertionError",
		"strictEqual: AssertionError",
		"deepStrictEqual: AssertionError",
		"strict types: AssertionError",
		"match: AssertionError",
		"throws: AssertionError",
		"a message of my own",
	}, "\n")
	if out != want {
		t.Errorf("assert output =\n%s\nwant\n%s", out, want)
	}
}

func TestBuffer(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		const b = Buffer.from("héllo")
		console.log(b.length, b.toString(), b instanceof Uint8Array, Buffer.isBuffer(b))
		console.log(b.toString("hex"))
		console.log(Buffer.from("68c3a96c6c6f", "hex").toString())
		console.log(Buffer.from("hi").toString("base64"), Buffer.from("aGk=", "base64").toString())
		console.log(Buffer.concat([Buffer.from("a"), Buffer.from("b")]).toString())
		console.log(Buffer.alloc(3).join(), Buffer.alloc(2, 7).join())
		console.log(Buffer.byteLength("héllo"), Buffer.from("ab").equals(Buffer.from("ab")))
		console.log(JSON.stringify(Buffer.from([1, 2])))
		const target = Buffer.alloc(4)
		console.log(target.write("hi"), target.toString("latin1", 0, 2))
	`)
	want := strings.Join([]string{
		"6 héllo true true",
		"68c3a96c6c6f",
		"héllo",
		"aGk= hi",
		"ab",
		"0,0,0 7,7",
		"6 true",
		`{"type":"Buffer","data":[1,2]}`,
		"2 hi",
	}, "\n")
	if out != want {
		t.Errorf("buffer output =\n%s\nwant\n%s", out, want)
	}
}

func TestServe(t *testing.T) {
	out, errOut := run(t, stdlib.Config{
		Fetch: &stdlib.Fetch{},
		Serve: &stdlib.Serve{Allow: func(string) error { return nil }},
	}, `
		;(async () => {
			const server = serve({port: 0}, async (req) => {
				const {pathname, searchParams} = new URL(req.url)
				switch (pathname) {
					case "/hello": return new Response("hello " + searchParams.get("who"))
					case "/echo": return new Response(await req.text(), {status: 201})
					case "/json": return Response.json({ok: true})
					case "/headers": return new Response(req.headers.get("x-sent"), {
						headers: {"x-answered": "yes"},
					})
					case "/boom": throw new Error("the handler exploded")
				}
				return new Response("nope", {status: 404})
			})
			const base = server.url

			console.log(await (await fetch(base + "/hello?who=world")).text())

			const echoed = await fetch(base + "/echo", {method: "POST", body: "sent up"})
			console.log(echoed.status, await echoed.text())

			const json = await (await fetch(base + "/json")).json()
			console.log(json.ok)

			const headed = await fetch(base + "/headers", {headers: {"x-sent": "value"}})
			console.log(await headed.text(), headed.headers.get("x-answered"))

			console.log((await fetch(base + "/missing")).status)
			console.log((await fetch(base + "/boom")).status)

			server.close()
		})()
	`)
	want := strings.Join([]string{
		"hello world",
		"201 sent up",
		"true",
		"value yes",
		"404",
		"500",
	}, "\n")
	if out != want {
		t.Errorf("serve output =\n%s\nwant\n%s", out, want)
	}
	// The handler's failure is reported rather than swallowed.
	if !strings.Contains(errOut, "the handler exploded") {
		t.Errorf("stderr = %q, want the handler's error", errOut)
	}
}

// A server holds the loop open, so a program whose last act is to listen does
// not exit -- and closing it lets the loop finish.
func TestServeHoldsTheLoop(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	var out bytes.Buffer
	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{
		Stdout: &out, Loop: loop,
		Serve: &stdlib.Serve{Allow: func(string) error { return nil }},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Eval(`
		const server = serve({port: 0}, () => new Response("x"))
		globalThis.stop = () => server.close()
		setTimeout(() => { console.log("still running"); stop() }, 20)
	`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := loop.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "still running" {
		t.Errorf("out = %q", got)
	}
}

// Listening is refused unless the host allows it, and the refusal says so.
func TestServeNeedsPermission(t *testing.T) {
	out, _ := run(t, stdlib.Config{
		Serve: &stdlib.Serve{Allow: func(addr string) error {
			return errors.New("not on " + addr)
		}},
	}, `
		try { serve({port: 0}, () => new Response("x")) }
		catch (e) { console.log("refused:", e.message) }
	`)
	if !strings.HasPrefix(out, "refused: not on ") {
		t.Errorf("out = %q", out)
	}
}

func TestCommands(t *testing.T) {
	out, _ := run(t, stdlib.Config{Run: &stdlib.Run{
		Allow: func(name string, args []string) error {
			if name != "echo" && name != "/bin/sh" && name != "false" {
				return errors.New(name + " is not allowed")
			}
			return nil
		},
	}}, `
		;(async () => {
			const cp = (await import("child_process")).default
			console.log(cp.execFileSync("echo", ["from a program"]).trim())

			const {stdout} = await cp.execFile("echo", ["awaited"])
			console.log(stdout.trim())

			const res = cp.spawnSync("/bin/sh", ["-c", "echo out; echo err 1>&2; exit 3"])
			console.log(res.status, res.stdout.trim(), res.stderr.trim())

			try { cp.execFileSync("false") } catch (e) { console.log("failed:", e.message) }
			try { cp.execFileSync("rm", ["-rf", "/"]) } catch (e) { console.log(e.message) }
		})()
	`)
	want := strings.Join([]string{
		"from a program",
		"awaited",
		"3 out err",
		"failed: false exited with 1",
		"rm is not allowed",
	}, "\n")
	if out != want {
		t.Errorf("child_process output =\n%s\nwant\n%s", out, want)
	}
}

// Without an Allow there is nothing a script may start, which is what a zero
// value means.
func TestCommandsRefusedByDefault(t *testing.T) {
	out, _ := run(t, stdlib.Config{Run: &stdlib.Run{}}, `
		import("child_process").then(({default: cp}) => {
			try { cp.execFileSync("echo", ["hi"]) } catch (e) { console.log(e.message) }
		})
	`)
	if want := "running programs is not allowed"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

// A program is given the environment the host chose, not the one this process
// happens to have.
func TestCommandsEnvironment(t *testing.T) {
	t.Setenv("QJS_SECRET", "do not pass this on")
	out, _ := run(t, stdlib.Config{Run: &stdlib.Run{
		Env:   map[string]string{"GIVEN": "yes"},
		Allow: func(string, []string) error { return nil },
	}}, `
		import("child_process").then(({default: cp}) => {
			console.log(cp.execFileSync("/bin/sh", ["-c", "echo [$GIVEN][$QJS_SECRET]"]).trim())
		})
	`)
	if want := "[yes][]"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestProcessEvents(t *testing.T) {
	out, _ := run(t, stdlib.Config{Process: &stdlib.Process{}}, `
		process.on("custom", (a, b) => console.log("heard", a, b))
		console.log(process.emit("custom", 1, 2))
		console.log(process.emit("nobody-listening"))
		process.on("unhandledRejection", (reason) => console.log("rejected:", reason.message))
		Promise.reject(new Error("nobody caught me"))
	`)
	want := "heard 1 2\ntrue\nfalse\nrejected: nobody caught me"
	if out != want {
		t.Errorf("process events output =\n%s\nwant\n%s", out, want)
	}
}

func TestCrypto(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		import("crypto").then((crypto) => {
			// The digests of "abc", which are written down in the standards.
			console.log(crypto.createHash("sha256").update("abc").digest("hex"))
			console.log(crypto.createHash("sha1").update("abc").digest("hex"))
			console.log(crypto.createHash("md5").update("abc").digest("hex"))
			console.log(crypto.createHash("sha3-256").update("abc").digest("hex"))
			console.log(crypto.createHash("SHA-256").update("abc").digest("base64"))

			// A hash fed in pieces is the hash of the whole.
			const whole = crypto.createHash("sha256").update("hello world").digest("hex")
			const parts = crypto.createHash("sha256").update("hello ").update("world").digest("hex")
			console.log(whole === parts)

			// RFC 2202's second HMAC-MD5 case, and the same key over bytes.
			console.log(crypto.createHmac("md5", "Jefe").update("what do ya want for nothing?").digest("hex"))
			console.log(crypto.createHmac("sha256", new Uint8Array([1,2,3])).update("x").digest().length)

			// RFC 6070's first PBKDF2 case.
			console.log(crypto.pbkdf2Sync("password", "salt", 1, 20, "sha1").toString("hex"))

			console.log(crypto.timingSafeEqual(new Uint8Array([1,2]), new Uint8Array([1,2])),
			            crypto.timingSafeEqual(new Uint8Array([1,2]), new Uint8Array([1,3])))
			console.log(crypto.randomBytes(8).length, crypto.getHashes().includes("sha512"))
		})
	`)
	want := strings.Join([]string{
		"ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
		"a9993e364706816aba3e25717850c26c9cd0d89d",
		"900150983cd24fb0d6963f7d28e17f72",
		"3a985da74fe225b2045c172d6bd390bd855f086e3e9d525b46bfe24511431532",
		"ungWv48Bz+pBQUDeXa4iI7ADYaOWF3qctBD/YfIAFa0=",
		"true",
		"750c783e6ab0b503eaa86e310a5db738",
		"32",
		"0c60c80f961f0e71f3a9b524af6012062fe037a6",
		"true false",
		"8 true",
	}, "\n")
	if out != want {
		t.Errorf("crypto output =\n%s\nwant\n%s", out, want)
	}
}

// The entropy is the host's, so a host that wants a program to run the same way
// twice can arrange it.
func TestCryptoUsesTheHostsRandomness(t *testing.T) {
	out, _ := run(t, stdlib.Config{Random: rand.New(rand.NewSource(1))}, `
		import("crypto").then((crypto) => {
			console.log(crypto.randomBytes(4).toString("hex"))
			console.log(crypto.randomInt(0, 100) < 100)
		})
	`)
	first := strings.SplitN(out, "\n", 2)[0]
	again, _ := run(t, stdlib.Config{Random: rand.New(rand.NewSource(1))}, `
		import("crypto").then((c) => console.log(c.randomBytes(4).toString("hex")))
	`)
	if first != strings.TrimSpace(again) {
		t.Errorf("the same source gave %q then %q", first, again)
	}
	if len(first) != 8 {
		t.Errorf("randomBytes(4) printed %q", first)
	}
}

// Deriving a key is slow on purpose, so the callback forms hand it back rather
// than pretending the work was free.
func TestCryptoAsyncForms(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		import("crypto").then((crypto) => {
			const {promisify} = {promisify: (fn) => (...a) =>
				new Promise((res, rej) => fn(...a, (e, v) => e ? rej(e) : res(v)))}
			promisify(crypto.pbkdf2)("password", "salt", 2, 20, "sha1")
				.then(key => console.log(key.toString("hex")))
			promisify(crypto.randomBytes)(4).then(b => console.log(b.length))
			promisify(crypto.hkdf)("sha256", "secret", "salt", "info", 16)
				.then(b => console.log(new Uint8Array(b).length))
		})
	`)
	want := "ea6c014dc72d6f8ccd1ed92ace1d41f0d8de8957\n4\n16"
	if out != want {
		t.Errorf("out =\n%s\nwant\n%s", out, want)
	}
}

// The web's subtle signs and derives with the same primitives, which is what a
// program written for a browser or a worker reaches for.
func TestSubtleCrypto(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			const enc = new TextEncoder()
			const key = await crypto.subtle.importKey(
				"raw", enc.encode("Jefe"), {name: "HMAC", hash: "SHA-256"}, true, ["sign", "verify"])

			const sig = await crypto.subtle.sign("HMAC", key, enc.encode("what do ya want for nothing?"))
			const hex = [...new Uint8Array(sig)].map(b => b.toString(16).padStart(2, "0")).join("")
			console.log(hex)
			console.log(await crypto.subtle.verify("HMAC", key, sig, enc.encode("what do ya want for nothing?")))
			console.log(await crypto.subtle.verify("HMAC", key, sig, enc.encode("something else")))

			// The key came back out the way it went in.
			const back = new Uint8Array(await crypto.subtle.exportKey("raw", key))
			console.log(new TextDecoder().decode(back))

			// RFC 6070 again, through the web's spelling of it.
			const base = await crypto.subtle.importKey(
				"raw", enc.encode("password"), "PBKDF2", false, ["deriveBits"])
			const bits = await crypto.subtle.deriveBits(
				{name: "PBKDF2", salt: enc.encode("salt"), iterations: 1, hash: "SHA-1"}, base, 160)
			console.log([...new Uint8Array(bits)].map(b => b.toString(16).padStart(2, "0")).join(""))

			// A key that may not leave is not handed over.
			try { await crypto.subtle.exportKey("raw", base) }
			catch (e) { console.log(e.constructor.name) }

			const derived = await crypto.subtle.deriveKey(
				{name: "HKDF", salt: enc.encode("salt"), info: enc.encode("info"), hash: "SHA-256"},
				await crypto.subtle.importKey("raw", enc.encode("secret"), "HKDF", false, ["deriveKey"]),
				{name: "HMAC", hash: "SHA-256", length: 256}, false, ["sign"])
			console.log((await crypto.subtle.sign("HMAC", derived, enc.encode("x"))).byteLength)

			// And the digest that was there before still is.
			console.log(new Uint8Array(await crypto.subtle.digest("SHA-256", enc.encode("abc")))[0])
		})()
	`)
	want := strings.Join([]string{
		"5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843",
		"true",
		"false",
		"Jefe",
		"0c60c80f961f0e71f3a9b524af6012062fe037a6",
		"TypeError",
		"32",
		"186",
	}, "\n")
	if out != want {
		t.Errorf("subtle output =\n%s\nwant\n%s", out, want)
	}
}

func TestStreams(t *testing.T) {
	out, errOut := run(t, stdlib.Config{}, `
		;(async () => {
			// A source that is pulled, read to the end.
			let pulls = 0
			const counted = new ReadableStream({
				pull(c) {
					pulls++
					if (pulls > 3) { c.close(); return }
					c.enqueue(pulls)
				},
			})
			const seen = []
			for await (const n of counted) seen.push(n)
			console.log(seen.join(","), pulls > 3)

			// A reader, by hand.
			const reader = ReadableStream.from(["a", "b"]).getReader()
			console.log((await reader.read()).value, (await reader.read()).value,
			            (await reader.read()).done)

			// A stream is locked while a reader holds it.
			const locked = ReadableStream.from([1])
			const r2 = locked.getReader()
			console.log(locked.locked)
			try { locked.getReader() } catch (e) { console.log(e.constructor.name) }
			r2.releaseLock()
			console.log(locked.locked)

			// Writing, with the sink seeing one chunk at a time in order.
			const written = []
			const sink = new WritableStream({
				write(chunk) {
					return new Promise(resolve => {
						// A slow sink still receives its chunks in order.
						setTimeout(() => { written.push(chunk); resolve() }, 0)
					})
				},
				close() { written.push("closed") },
			})
			const writer = sink.getWriter()
			writer.write("one"); writer.write("two")
			await writer.close()
			console.log(written.join(" "))

			// Through a transform and out the other side.
			const upper = new TransformStream({
				transform(chunk, c) { c.enqueue(chunk.toUpperCase()) },
				flush(c) { c.enqueue("!") },
			})
			const pieces = []
			await ReadableStream.from(["ab", "cd"])
				.pipeThrough(upper)
				.pipeTo(new WritableStream({write: (c) => { pieces.push(c) }}))
			console.log(pieces.join(""))

			// Text in, bytes out, and back again -- including a character split
			// across two chunks.
			const bytes = new TextEncoder().encode("héllo")
			const halves = ReadableStream.from([bytes.slice(0, 2), bytes.slice(2)])
			let text = ""
			for await (const s of halves.pipeThrough(new TextDecoderStream())) text += s
			console.log(text, text.length)

			// tee gives two streams that see the same chunks.
			const [a, b] = ReadableStream.from([1, 2, 3]).tee()
			const drain = async (s) => { const o = []; for await (const v of s) o.push(v); return o }
			console.log((await Promise.all([drain(a), drain(b)])).map(x => x.join("")).join(" "))

			// An error travels to whoever was reading.
			const broken = new ReadableStream({start(c) { c.error(new Error("the source failed")) }})
			try { await broken.getReader().read() } catch (e) { console.log(e.message) }

			// Cancelling tells the source.
			let told = null
			const cancellable = new ReadableStream({cancel(reason) { told = reason }})
			await cancellable.cancel("no longer wanted")
			console.log(told)
		})()
	`)
	want := strings.Join([]string{
		"1,2,3 true",
		"a b true",
		"true",
		"TypeError",
		"false",
		"one two closed",
		"ABCD!",
		"héllo 5",
		"123 123",
		"the source failed",
		"no longer wanted",
	}, "\n")
	if out != want {
		t.Errorf("streams output =\n%s\nwant\n%s\nstderr: %s", out, want, errOut)
	}
}

// A slow consumer holds a fast producer back, which is the point of a stream
// rather than an array.
func TestStreamsBackpressure(t *testing.T) {
	out, _ := run(t, stdlib.Config{}, `
		;(async () => {
			let made = 0
			const source = new ReadableStream({
				pull(c) { c.enqueue(++made) },
			}, {highWaterMark: 2})

			// Nothing has been asked for yet beyond the mark.
			await new Promise(r => setTimeout(r, 0))
			console.log(made <= 3, made > 0)

			const reader = source.getReader()
			await reader.read()
			await reader.read()
			console.log(made <= 5)

			// A writable stream reports how much room is left.
			const w = new WritableStream({write: () => new Promise(() => {})}, {highWaterMark: 2})
			const writer = w.getWriter()
			console.log(writer.desiredSize)
			writer.write("a")
			console.log(writer.desiredSize)
			writer.write("b")
			console.log(writer.desiredSize)

			// ready is only settled when there is room again.
			let free = false
			writer.ready.then(() => { free = true })
			await new Promise(r => setTimeout(r, 0))
			console.log("room:", free)
		})()
	`)
	want := "true true\ntrue\n2\n1\n0\nroom: false"
	if out != want {
		t.Errorf("backpressure output =\n%s\nwant\n%s", out, want)
	}
}

// A body is bytes or a stream of them, and the two are the same body seen from
// different ends.
func TestStreamingBodies(t *testing.T) {
	out, errOut := run(t, stdlib.Config{
		Fetch: &stdlib.Fetch{},
		Serve: &stdlib.Serve{Allow: func(string) error { return nil }},
	}, `
		;(async () => {
			// A response built from a stream.
			const made = new Response(ReadableStream.from([
				new TextEncoder().encode("one "),
				new TextEncoder().encode("two"),
			]))
			console.log(await made.text())

			// And read the other way, a chunk at a time.
			const chunks = []
			for await (const c of new Response("hello").body) chunks.push(c.length)
			console.log(chunks.join(","))

			// A body with nothing in it is null, not an empty stream.
			console.log(new Response(null).body === null)

			// Reading a body twice is an error whichever way it is read.
			const once = new Response("x")
			await once.text()
			try { await once.text() } catch (e) { console.log(e.constructor.name) }

			// A clone of an unread stream body gives both halves.
			const original = new Response(ReadableStream.from([new Uint8Array([104, 105])]))
			const copy = original.clone()
			console.log(await original.text(), await copy.text())

			// Over the network: a handler that answers with a stream, read back
			// through fetch as a stream.
			const server = serve({port: 0}, async (request) => {
				const sent = await request.text()
				return new Response(ReadableStream.from(
					[...sent].map(ch => new TextEncoder().encode(ch.toUpperCase()))))
			})
			const res = await fetch(server.url, {method: "POST", body: "abc"})
			let got = ""
			for await (const chunk of res.body) got += new TextDecoder().decode(chunk)
			console.log(got)

			// A request body given as a stream is gathered before it is sent.
			const echo = await fetch(server.url, {
				method: "POST",
				body: ReadableStream.from([new TextEncoder().encode("de")]),
			})
			console.log(await echo.text())
			server.close()
		})()
	`)
	want := strings.Join([]string{
		"one two",
		"5",
		"true",
		"TypeError",
		"hi hi",
		"ABC",
		"DE",
	}, "\n")
	if out != want {
		t.Errorf("streaming bodies =\n%s\nwant\n%s\nstderr: %s", out, want, errOut)
	}
}

func TestCompression(t *testing.T) {
	out, errOut := run(t, stdlib.Config{}, `
		;(async () => {
			const zlib = (await import("zlib")).default
			const text = "the same sentence, over and over. ".repeat(50)

			// Every framing survives the round trip, and is smaller than what
			// went in.
			for (const [pack, unpack] of [["gzipSync", "gunzipSync"],
			                              ["deflateSync", "inflateSync"],
			                              ["deflateRawSync", "inflateRawSync"]]) {
				const packed = zlib[pack](text)
				const back = zlib[unpack](packed).toString()
				console.log(pack, back === text, packed.length < text.length)
			}

			// gzip's magic number, so it really is the format it says.
			const gz = zlib.gzipSync("x")
			console.log(gz[0] === 0x1f && gz[1] === 0x8b)

			// unzip takes either framing.
			console.log(zlib.unzipSync(zlib.gzipSync("a")).toString(),
			            zlib.unzipSync(zlib.deflateSync("b")).toString())

			// Through the streams, where the data is compressed as it goes.
			const gather = async (stream) => {
				const chunks = []
				let total = 0
				for await (const c of stream) { chunks.push(c); total += c.length }
				const out = new Uint8Array(total)
				let at = 0
				for (const c of chunks) { out.set(c, at); at += c.length }
				return out
			}
			const source = ReadableStream.from(
				[..."abcdefghij"].map(c => new TextEncoder().encode(c.repeat(100))))
			const packed = await gather(source.pipeThrough(new CompressionStream("gzip")))
			const unpacked = new TextDecoder().decode(await gather(
				ReadableStream.from([packed]).pipeThrough(new DecompressionStream("gzip"))))
			console.log(packed.length < 1000, unpacked.length, unpacked.slice(0, 3))

			// Something that is not compressed data says so rather than
			// producing nonsense.
			try { zlib.gunzipSync(new Uint8Array([1, 2, 3])) }
			catch (e) { console.log("refused:", e.constructor.name) }

			// And the callback form node code is written against.
			zlib.gzip("cb", (err, packed) =>
				zlib.gunzip(packed, (err2, back) => console.log("callback:", back.toString())))
		})()
	`)
	want := strings.Join([]string{
		"gzipSync true true",
		"deflateSync true true",
		"deflateRawSync true true",
		"true",
		"a b",
		"true 1000 aaa",
		"refused: Error",
		"callback: cb",
	}, "\n")
	if out != want {
		t.Errorf("compression output =\n%s\nwant\n%s\nstderr: %s", out, want, errOut)
	}
}
