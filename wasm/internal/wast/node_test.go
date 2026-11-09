package wast

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// nodeCheck is the script that compiles each module with
// new WebAssembly.Module and prints one JSON result per module.
const nodeCheck = `
const fs = require('fs');
const path = require('path');
const [dir, count] = [process.argv[2], Number(process.argv[3])];
for (let i = 0; i < count; i++) {
  const bytes = fs.readFileSync(path.join(dir, i + '.wasm'));
  let r;
  try {
    new WebAssembly.Module(bytes);
    r = {i, ok: true};
  } catch (e) {
    r = {i, ok: false, err: String(e && e.message)};
  }
  process.stdout.write(JSON.stringify(r) + '\n');
}
`

// v8Limits matches the errors with which V8 rejects a valid module for
// exceeding one of its implementation limits rather than the
// specification's.
var v8Limits = regexp.MustCompile(`(initial|maximum) (memory|table) size \(\d+ (pages|elements)\) is larger than implementation limit`)

// TestNodeAcceptsCompiledModules compiles the spec suites' text modules and
// has Node (V8) compile each: every module a script expects to be valid
// must be accepted, and every module of assert_invalid rejected. Modules V8
// rejects for its implementation limits are listed, not failed.
//
//	QUICKJS_COMPARE_NODE_WASM=1 WASM_SPEC_DIR=/path/to/spec \
//	  WASM_THREADS_DIR=/path/to/threads go test ./wasm/internal/wast \
//	  -run TestNodeAcceptsCompiledModules -v
func TestNodeAcceptsCompiledModules(t *testing.T) {
	if os.Getenv("QUICKJS_COMPARE_NODE_WASM") == "" {
		t.Skip("set QUICKJS_COMPARE_NODE_WASM=1 to compare with Node")
	}
	files := specFiles(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	dir := t.TempDir()
	var ms []scriptModule
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		name, _ := filepath.Rel(filepath.Dir(filepath.Dir(file)), file)
		cmds, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, m := range modules(name, cmds) {
			if m.module.Kind == BinaryModule || m.expect == expectMalformed {
				continue
			}
			bin, err := m.module.Binary()
			if err != nil {
				t.Fatalf("%s:%d: %v", name, m.line, err)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(len(ms))+".wasm"), bin, 0o666); err != nil {
				t.Fatal(err)
			}
			ms = append(ms, m)
		}
	}
	script := filepath.Join(dir, "check.js")
	if err := os.WriteFile(script, []byte(nodeCheck), 0o666); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(node, script, dir, fmt.Sprint(len(ms)))
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, stderr.String())
	}
	var valid, invalid, limits, failures int
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	seen := 0
	for sc.Scan() {
		var r struct {
			I   int
			OK  bool
			Err string
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("node output %q: %v", sc.Text(), err)
		}
		seen++
		m := ms[r.I]
		where := fmt.Sprintf("%s:%d", m.file, m.line)
		switch {
		case m.expect == expectValid && r.OK:
			valid++
		case m.expect == expectValid && v8Limits.MatchString(r.Err):
			limits++
			t.Logf("V8 limit: %s: %s", where, r.Err)
		case m.expect == expectValid:
			failures++
			t.Errorf("%s: valid module rejected: %s", where, r.Err)
		case !r.OK:
			invalid++
		default:
			failures++
			t.Errorf("%s: invalid module accepted", where)
		}
	}
	if seen != len(ms) {
		t.Fatalf("node checked %d of %d modules\n%s", seen, len(ms), strings.TrimSpace(stderr.String()))
	}
	t.Logf("%d modules: %d valid accepted, %d invalid rejected, %d rejected for V8's limits, %d disagreements",
		len(ms), valid, invalid, limits, failures)
}
