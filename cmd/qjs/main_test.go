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
