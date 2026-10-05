package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// exec runs the command line as main would, without starting a process.
func exec(t *testing.T, stdin string, argv ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(argv, strings.NewReader(stdin), &o, &e)
	return code, o.String(), e.String()
}

func TestRunsAnExpression(t *testing.T) {
	code, out, errOut := exec(t, "", "-e", `console.log(1 + 1)`)
	if code != 0 || out != "2\n" || errOut != "" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestRunsAFile(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.js")
	if err := os.WriteFile(script, []byte(`console.log("from a file", process.argv.slice(2).join())`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := exec(t, "", script, "a", "b")
	if code != 0 || out != "from a file a,b\n" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

// TestStackNamesTheFile pins that a script's stack trace says which file each
// frame is in, and what -e and standard input are called.
func TestStackNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "s.js")
	src := "function f() { throw new Error(\"e\") }\nf()\n"
	if err := os.WriteFile(script, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, errOut := exec(t, "", script)
	if want := "at f (" + script + ":1:22)"; !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want it to contain %q", errOut, want)
	}
	_, _, errOut = exec(t, "", "-e", src)
	if want := "at f (<cmdline>:1:22)"; !strings.Contains(errOut, want) {
		t.Errorf("-e stderr = %q, want it to contain %q", errOut, want)
	}
	_, _, errOut = exec(t, src, "-")
	if want := "at <stdin>:2:1"; !strings.Contains(errOut, want) {
		t.Errorf("stdin stderr = %q, want it to contain %q", errOut, want)
	}
}

func TestRunsStdin(t *testing.T) {
	code, out, _ := exec(t, `console.log("piped")`, "-")
	if code != 0 || out != "piped\n" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

// A file that imports is run as a module without being told to, since it could
// not run as a script at all.
func TestRunsAModule(t *testing.T) {
	dir := t.TempDir()
	dep := filepath.Join(dir, "dep.js")
	main := filepath.Join(dir, "main.js")
	if err := os.WriteFile(dep, []byte(`export const n = 41`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte(
		"import {n} from \"./dep.js\"\nconsole.log(n + 1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := exec(t, "", main)
	if code != 0 || out != "42\n" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// An import() is resolved against the file it is written in, as a static import
// is: a module's in a directory of its own, and a script's. It was resolved
// against the working directory.
func TestDynamicImportIsRelativeToItsFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("lib/b.js", `export const b = "b"`)
	write("lib/a.js", `export const load = () => import("./b.js")`)
	main := write("main.js", "import {load} from \"./lib/a.js\"\nconsole.log((await load()).b)\n")
	script := write("script.cjs", `import("./lib/b.js").then(m => console.log(m.b + " from a script"))`)
	for _, c := range []struct{ file, want string }{
		{main, "b\n"},
		{script, "b from a script\n"},
	} {
		code, out, errOut := exec(t, "", c.file)
		if code != 0 || out != c.want {
			t.Errorf("%s: code=%d out=%q err=%q", filepath.Base(c.file), code, out, errOut)
		}
	}
}

// A bare specifier that is not a module the host installed says so, rather than
// looking for a file with that name.
func TestBareSpecifierIsExplained(t *testing.T) {
	code, _, errOut := exec(t, "", "-m", "-e", `import "lodash"`)
	if code == 0 || !strings.Contains(errOut, "lodash") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestExitCodeOnUncaughtError(t *testing.T) {
	code, _, errOut := exec(t, "", "-e", `null.x`)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "TypeError") {
		t.Errorf("stderr = %q, want a TypeError", errOut)
	}
}

func TestSyntaxCheck(t *testing.T) {
	if code, _, _ := exec(t, "", "--check", "-e", `const a = 1`); code != 0 {
		t.Errorf("a good program checked as %d", code)
	}
	code, _, errOut := exec(t, "", "--check", "-e", `const = `)
	if code != 1 || !strings.Contains(errOut, "SyntaxError") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// Nothing outside the process is reachable unless the command line said so.
func TestPermissionsAreOff(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ := exec(t, "", "-e", `
		import("fs").then(({default: fs}) => {
			try { fs.readFileSync(`+quote(secret)+`, "utf8"); console.log("READ") }
			catch (e) { console.log("refused") }
		})
	`)
	if code != 0 || strings.TrimSpace(out) != "refused" {
		t.Errorf("reading without permission: code=%d out=%q", code, out)
	}

	code, out, _ = exec(t, "", "-e", `
		fetch("http://127.0.0.1:1/x").then(() => console.log("SENT"), e => console.log("refused"))
	`)
	if code != 0 || strings.TrimSpace(out) != "refused" {
		t.Errorf("fetching without permission: code=%d out=%q", code, out)
	}

	code, out, _ = exec(t, "", "-e", `console.log(Object.keys(process.env).length)`)
	if code != 0 || strings.TrimSpace(out) != "0" {
		t.Errorf("the environment without permission: code=%d out=%q", code, out)
	}
}

// TestWorkers pins that qjs starts workers, as node does, from a file URL, a
// path and a data URL; that a worker imports beside itself; and that it is
// given what the program was given and no more.
func TestWorkers(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"helper.mjs": `export const twice = (n) => n * 2;`,
		"w.mjs": `
			import { parentPort, workerData } from "node:worker_threads";
			import { twice } from "./helper.mjs";
			let fs = "refused";
			try { (await import("node:fs")).default.readFileSync("x") } catch (e) { fs = e.message }
			parentPort.postMessage(twice(workerData) + " " + fs);`,
		"main.mjs": `
			import { Worker } from "node:worker_threads";
			const run = (spec, data) => new Promise((resolve) => {
				const w = new Worker(spec, { workerData: data });
				w.on("message", (m) => console.log(m));
				w.on("error", (e) => console.log("error", e.message));
				w.on("exit", resolve);
			});
			await run(new URL("./w.mjs", import.meta.url), 1);
			await run(` + quote(filepath.Join(dir, "w.mjs")) + `, 2);
			await run("data:text/javascript,import { parentPort } from 'node:worker_threads'; parentPort.postMessage('from data')");
			await run("./nowhere.mjs");
			new globalThis.Worker(new URL("./w.mjs", import.meta.url)).terminate();`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := exec(t, "", filepath.Join(dir, "main.mjs"))
	nowhere, _ := filepath.Abs("nowhere.mjs")
	want := strings.Join([]string{
		"2 filesystem access is not allowed: run qjs with --allow-read",
		"4 filesystem access is not allowed: run qjs with --allow-read",
		"from data",
		"error Cannot find module '" + nowhere + "'",
	}, "\n") + "\n"
	if code != 0 || out != want {
		t.Errorf("code=%d out=\n%s\nwant\n%s\nerr=%s", code, out, want, errOut)
	}
}

func TestAllowRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := exec(t, "", "--allow-read="+dir, "-e", `
		import("fs").then(({default: fs}) => console.log(fs.readFileSync("/a.txt", "utf8")))
	`)
	if code != 0 || strings.TrimSpace(out) != "contents" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}

	// Reading is allowed; writing still is not.
	code, out, _ = exec(t, "", "--allow-read="+dir, "-e", `
		import("fs").then(({default: fs}) => {
			try { fs.writeFileSync("/b.txt", "no"); console.log("WROTE") }
			catch (e) { console.log("refused") }
		})
	`)
	if code != 0 || strings.TrimSpace(out) != "refused" {
		t.Errorf("writing with only --allow-read: code=%d out=%q", code, out)
	}
}

func TestAllowWrite(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := exec(t, "", "--allow-write="+dir, "-e", `
		import("fs").then(({default: fs}) => {
			fs.writeFileSync("/w.txt", "written")
			console.log(fs.readFileSync("/w.txt", "utf8"))
		})
	`)
	if code != 0 || strings.TrimSpace(out) != "written" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "w.txt")); err != nil || string(b) != "written" {
		t.Errorf("the file on disk = %q, %v", b, err)
	}
}

func TestAllowEnv(t *testing.T) {
	t.Setenv("QJS_TEST_VALUE", "from the environment")
	code, out, _ := exec(t, "", "--allow-env", "-e", `console.log(process.env.QJS_TEST_VALUE)`)
	if code != 0 || strings.TrimSpace(out) != "from the environment" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestTimersRunToCompletion(t *testing.T) {
	code, out, _ := exec(t, "", "-e", `
		setTimeout(() => console.log("second"), 5)
		Promise.resolve().then(() => console.log("first"))
	`)
	if code != 0 || out != "first\nsecond\n" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestTimeoutStops(t *testing.T) {
	code, _, errOut := exec(t, "", "--timeout", "200ms", "-e", `while (true) {}`)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "deadline") && !strings.Contains(errOut, "interrupted") {
		t.Errorf("stderr = %q, want it to mention the deadline", errOut)
	}
}

func TestMemoryLimitAndStackSize(t *testing.T) {
	// The flags are accepted and bound the runtime; what they bound is the
	// engine's business, which its own tests cover.
	if code, _, errOut := exec(t, "", "--memory-limit", "64m", "--stack-size", "8192",
		"-e", `console.log("ran")`); code != 0 {
		t.Errorf("code=%d err=%q", code, errOut)
	}
	if code, _, _ := exec(t, "", "--memory-limit", "banana", "-e", `1`); code != 2 {
		t.Error("a bad size should have been refused")
	}
}

// Without code generation eval is not there at all, and the Function
// constructor -- which cannot be removed without changing what every function
// inherits from -- refuses to build one.
func TestNoCodeGeneration(t *testing.T) {
	code, out, _ := exec(t, "", "--no-code-generation", "-e", `
		console.log(typeof eval)
		try { eval("1") } catch (e) { console.log(e.constructor.name) }
		try { new Function("return 1") } catch (e) { console.log(e.constructor.name) }
	`)
	if code != 0 || strings.TrimSpace(out) != "undefined\nReferenceError\nTypeError" {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestNodeQuirks(t *testing.T) {
	code, out, errOut := exec(t, "", "--node-quirks", "-e", `
		console.log(new Temporal.PlainTime(0).toLocaleString("en", {
			hour12: false,
		}))
	`)
	if code != 0 || strings.TrimSpace(out) != "12:00:00 AM" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestHelpAndVersion(t *testing.T) {
	if code, _, _ := exec(t, "", "--help"); code != 0 {
		t.Errorf("--help exited %d", code)
	}
	if code, _, _ := exec(t, "", "--version"); code != 0 {
		t.Errorf("--version exited %d", code)
	}
	if code, _, errOut := exec(t, "", "--nonsense"); code != 2 || !strings.Contains(errOut, "unknown option") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// The prompt evaluates what is typed, keeps an unfinished line, and prints what
// an expression produced.
func TestREPL(t *testing.T) {
	input := strings.Join([]string{
		`const a = 20`,
		`a + 1`,
		`function twice(n) {`,
		`  return n * 2`,
		`}`,
		`twice(21)`,
		`_ + 0`,
		`"text"`,
		`undefined`,
		`({a: [1, 2]})`,
		`.exit`,
	}, "\n") + "\n"

	code, out, errOut := exec(t, input)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"21", "42", "'text'", "{ a: [ 1, 2 ] }"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain %q", out, want)
		}
	}
	// undefined is not printed, as at any prompt.
	if strings.Contains(out, "undefined") {
		t.Errorf("output printed undefined: %q", out)
	}
}

func TestREPLReportsErrors(t *testing.T) {
	code, _, errOut := exec(t, "null.x\n1 + )\n.exit\n")
	if code != 0 {
		t.Errorf("the prompt exited %d", code)
	}
	if !strings.Contains(errOut, "TypeError") || !strings.Contains(errOut, "SyntaxError") {
		t.Errorf("stderr = %q, want both errors", errOut)
	}
}

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{"1024", 1024, false},
		{"64m", 64 << 20, false},
		{"2G", 2 << 30, false},
		{"8k", 8 << 10, false},
		{"", 0, true},
		{"banana", 0, true},
	} {
		got, err := parseSize(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("parseSize(%q) should have failed", tc.in)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("parseSize(%q) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
}

func TestLooksLikeModule(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want bool
	}{
		{`import x from "y"`, true},
		{`export const a = 1`, true},
		{"const a = 1\nexport {a}", true},
		{`const s = "import x from y"`, false},
		{`console.log("export const")`, false},
		{`import("dynamic")`, false},
	} {
		if got := looksLikeModule(tc.src); got != tc.want {
			t.Errorf("looksLikeModule(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// quote makes a JavaScript string literal out of a path.
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

// A rejection nothing took is reported where it happened, and the exit code
// says the program failed.
func TestUnhandledRejectionIsReported(t *testing.T) {
	code, _, errOut := exec(t, "", "-e", `Promise.reject(new Error("nobody caught me"))`)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(errOut, "nobody caught me") {
		t.Errorf("stderr = %q", errOut)
	}

	// One that is caught says nothing.
	code, _, errOut = exec(t, "", "-e", `Promise.reject(new Error("caught")).catch(() => {})`)
	if code != 0 || errOut != "" {
		t.Errorf("code=%d err=%q", code, errOut)
	}

	// Including one from an async function, which is where they mostly come
	// from.
	code, _, errOut = exec(t, "", "-e", `(async () => { throw new Error("async failure") })()`)
	if code != 1 || !strings.Contains(errOut, "async failure") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// A module is told where it came from, in the forms node offers.
func TestImportMetaInTheCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "m.mjs")
	urlPath := filepath.ToSlash(script)
	if runtime.GOOS == "windows" {
		urlPath = "/" + urlPath
	}
	wantURL := (&url.URL{Scheme: "file", Path: urlPath}).String()
	source := fmt.Sprintf(`console.log(import.meta.url === %s, import.meta.filename === %s, import.meta.dirname === %s)`,
		quote(wantURL), quote(script), quote(dir))
	if err := os.WriteFile(script, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := exec(t, "", script)
	if code != 0 || strings.TrimSpace(out) != "true true true" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestFileURLOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows path syntax")
	}
	for path, want := range map[string]string{
		`C:\Program Files\qjs\main.mjs`: `file:///C:/Program%20Files/qjs/main.mjs`,
		`\\server\share\main.mjs`:       `file://server/share/main.mjs`,
	} {
		if got := fileURL(path); got != want {
			t.Errorf("fileURL(%q) = %q, want %q", path, got, want)
		}
	}
}

// Starting a program is refused unless the command line asked for it, and the
// refusal names the flag that would grant it.
func TestAllowRun(t *testing.T) {
	code, out, _ := exec(t, "", "-e", `
		import("child_process").then(({default: cp}) => {
			try { cp.execFileSync("echo", ["ran"]) } catch (e) { console.log(e.message) }
		})
	`)
	if code != 0 || !strings.Contains(out, "--allow-run") {
		t.Errorf("code=%d out=%q, want the flag named", code, out)
	}

	program, args := "echo", `["ran"]`
	if runtime.GOOS == "windows" {
		program, args = "cmd.exe", `["/d", "/s", "/c", "echo ran"]`
	}
	source := fmt.Sprintf(`
		import("child_process").then(({default: cp}) => {
			console.log(cp.execFileSync(%q, %s).trim())
		})
	`, program, args)
	code, out, errOut := exec(t, "", "--allow-run", "-e", source)
	if code != 0 || strings.TrimSpace(out) != "ran" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// A program is given only the environment the command line allowed, so a script
// refused the environment cannot read it through a program it starts.
func TestAllowRunDoesNotLeakTheEnvironment(t *testing.T) {
	t.Setenv("QJS_TEST_SECRET", "not for the script")
	command := `printf '[%s]\n' "$QJS_TEST_SECRET"`
	if runtime.GOOS == "windows" {
		command = `echo [%QJS_TEST_SECRET%]`
	}
	source := fmt.Sprintf(`
		import("child_process").then(({default: cp}) => {
			console.log(cp.execSync(%q).trim())
		})
	`, command)
	code, out, _ := exec(t, "", "--allow-run", "-e", source)
	want := "[]"
	if runtime.GOOS == "windows" {
		// cmd.exe leaves references to absent variables intact; importantly,
		// the value from the parent environment is not present.
		want = "[%QJS_TEST_SECRET%]"
	}
	if code != 0 || strings.TrimSpace(out) != want {
		t.Errorf("code=%d out=%q, want the secret withheld", code, out)
	}

	code, out, _ = exec(t, "", "--allow-run", "--allow-env", "-e", source)
	if code != 0 || strings.TrimSpace(out) != "[not for the script]" {
		t.Errorf("with --allow-env: code=%d out=%q", code, out)
	}
}

// Listening is network access, and the same flag that grants fetching grants it.
func TestAllowNetServes(t *testing.T) {
	code, out, _ := exec(t, "", "-e", `
		try { serve({port: 0}, () => new Response("x")) }
		catch (e) { console.log(e.message) }
	`)
	if code != 0 || !strings.Contains(out, "--allow-net") {
		t.Errorf("code=%d out=%q, want the flag named", code, out)
	}

	code, out, errOut := exec(t, "", "--allow-net", "-e", `
		;(async () => {
			const server = serve({port: 0}, (req) =>
				new Response("served " + new URL(req.url).pathname))
			console.log(await (await fetch(server.url + "/here")).text())
			server.close()
		})()
	`)
	if code != 0 || strings.TrimSpace(out) != "served /here" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// Looking a name up is network access, granted by the same flag, for the
// names it lists and what is under them; refused, it says which flag.
func TestAllowNetLooksUp(t *testing.T) {
	src := `
		import("dns").then(async ({promises: dns}) => {
			try {
				const all = await dns.lookup("localhost", {all: true})
				console.log(all.some(a => a.address === "127.0.0.1" || a.address === "::1"))
			} catch (e) { console.log(e.code, e.cause.message) }
		})
	`
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "EREFUSED looking up localhost is not allowed: run qjs with --allow-net"},
		{[]string{"--allow-net=example.com"}, "EREFUSED looking up localhost is not allowed: pass --allow-net=localhost"},
		{[]string{"--allow-net=localhost"}, "true"},
		{[]string{"--allow-net"}, "true"},
	} {
		code, out, errOut := exec(t, "", append(tc.args, "-e", src)...)
		if code != 0 || strings.TrimSpace(out) != tc.want {
			t.Errorf("%v: code=%d out=%q err=%q, want %q", tc.args, code, out, errOut, tc.want)
		}
	}
}

// --allow-run can name the programs it allows, in which case nothing else may
// be started.
func TestAllowRunList(t *testing.T) {
	program, args := "echo", `["allowed"]`
	if runtime.GOOS == "windows" {
		program, args = "cmd.exe", `["/d", "/s", "/c", "echo allowed"]`
	}
	source := fmt.Sprintf(`
		import("child_process").then(({default: cp}) => {
			console.log(cp.execFileSync(%q, %s).trim())
			try { cp.execFileSync("ls") } catch (e) { console.log(e.message) }
		})
	`, program, args)
	code, out, errOut := exec(t, "", "--allow-run="+program, "-e", source)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || lines[0] != "allowed" || !strings.Contains(lines[1], "--allow-run=ls") {
		t.Errorf("out = %q", out)
	}
}

// A socket needs the same permission the rest of the network does.
func TestAllowNetSockets(t *testing.T) {
	code, out, _ := exec(t, "", "-e", `
		try { new WebSocket("ws://example.com") } catch (e) { console.log(e.message) }
	`)
	if code != 0 || !strings.Contains(out, "--allow-net") {
		t.Errorf("code=%d out=%q, want the flag named", code, out)
	}

	code, out, errOut := exec(t, "", "--allow-net", "-e", `
		const server = serve({port: 0}, (request) => {
			const {socket, response} = upgradeWebSocket(request)
			socket.onmessage = (e) => { socket.send(e.data.toUpperCase()); socket.close() }
			return response
		})
		const ws = new WebSocket(server.url.replace("http", "ws"))
		ws.onopen = () => ws.send("quiet")
		ws.onmessage = (e) => console.log("heard", e.data)
		ws.onclose = () => server.close()
	`)
	if code != 0 || strings.TrimSpace(out) != "heard QUIET" {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
}

// The prompt accepts a top-level await, which is what a prompt is mostly used
// for once anything is asynchronous.
func TestREPLAwait(t *testing.T) {
	input := strings.Join([]string{
		`const answer = await Promise.resolve(41)`,
		`answer + 1`,
		`await new Promise(r => setTimeout(() => r("slept"), 5))`,
		`for (const x of [1, 2]) { await null; console.log("saw", x) }`,
		`await Promise.reject(new Error("no good"))`,
		`"still going"`,
		`.exit`,
	}, "\n") + "\n"

	code, out, errOut := exec(t, input)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	for _, want := range []string{"41", "42", "'slept'", "saw 1", "saw 2", "'still going'"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// A rejection is reported once, not once as uncaught and again as the
	// prompt's own error.
	if n := strings.Count(errOut, "no good"); n != 1 {
		t.Errorf("the rejection was reported %d times:\n%s", n, errOut)
	}
}

func TestAwaitedDeclarationsPersist(t *testing.T) {
	code, out, _ := exec(t, "const kept = await Promise.resolve(7)\nkept * 6\n.exit\n")
	if code != 0 || !strings.Contains(out, "42") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

// An unfinished input is still merely unfinished, even where it contains await.
func TestREPLUnfinishedAwait(t *testing.T) {
	code, out, errOut := exec(t, "const v = await Promise.resolve({\n  a: 1,\n})\nv.a\n.exit\n")
	if code != 0 || !strings.Contains(out, "1") {
		t.Errorf("code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(errOut, "SyntaxError") {
		t.Errorf("an unfinished line was reported as an error: %q", errOut)
	}
}

// TestProcessEnding pins how a program's end is decided, as node decides it:
// a rejection the script listens for with process.on("unhandledRejection")
// is handled, where it failed the program regardless; and a program that
// set process.exitCode ends with it, where it ended with 0 (KI-49). One that
// does not listen still fails.
func TestProcessEnding(t *testing.T) {
	code, out, errOut := exec(t, "", "-e", `process.on("unhandledRejection", (r) => console.log("handled", r));
		Promise.reject("nope"); process.exitCode = 3;`)
	if code != 3 || out != "handled nope\n" || errOut != "" {
		t.Errorf("listening: code=%d out=%q err=%q", code, out, errOut)
	}
	code, _, errOut = exec(t, "", "-e", `Promise.reject("nope")`)
	if code != 1 || !strings.Contains(errOut, "uncaught (in promise)") {
		t.Errorf("not listening: code=%d err=%q", code, errOut)
	}
}

// TestFileAndDataURLs pins how qjs reads file and data URLs, as node's
// fileURLToPath and import do, on Windows and elsewhere: localhost, in any
// case, is no host; another host is a UNC server on Windows and refused
// elsewhere; file:C:/x is file:///C:/x; an encoded separator is refused; a
// data URL's ;BASE64 is in any case, its padding optional, and a % that
// begins no escape is kept (KI-50).
func TestFileAndDataURLs(t *testing.T) {
	for _, c := range []struct{ url, posix, windows string }{
		{"file://LOCALHOST/tmp/x.js", "/tmp/x.js", "error"},
		{"file://127.0.0.1/share/x.js", "error", `\\127.0.0.1\share\x.js`},
		{"file:///C:", "/C:", "C:"},
		{"file:///C:/x/y.js", "/C:/x/y.js", `C:\x\y.js`},
		{"file:C:/x/y.js", "/C:/x/y.js", `C:\x\y.js`},
		{"file:///tmp/a%2Fb.js", "error", "error"},
		{"file:///tmp/a%5Cb.js", `/tmp/a\b.js`, "error"},
		{"file:///tmp/a%20b.js", "/tmp/a b.js", "error"},
	} {
		for _, w := range []bool{false, true} {
			got, err := fileURLPath(c.url, w)
			if err != nil {
				got = "error"
			}
			if want := map[bool]string{false: c.posix, true: c.windows}[w]; got != want {
				t.Errorf("%s (windows %v) = %q, want %q", c.url, w, got, want)
			}
		}
	}
	for spec, want := range map[string]string{
		"data:text/javascript;BASE64,ZXhwb3J0IGRlZmF1bHQgMQ":     "export default 1",
		"data:text/javascript;base64,ZXhwb3J0IGRlZmF1bHQgMg==":   "export default 2",
		`data:text/javascript,export default "a%zzb%41"`:         `export default "a%zzbA"`,
		"data:text/javascript;base64 ,ZXhwb3J0IGRlZmF1bHQgMw==":  "export default 3",
		"data:text/javascript;base64,ZXhw b3J0 IGRlZmF1bHQgNA==": "export default 4",
	} {
		if got, err := decodeDataURL(spec); err != nil || got != want {
			t.Errorf("%s = %q, %v; want %q", spec, got, err, want)
		}
	}
}
