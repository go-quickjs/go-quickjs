package vm

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// regLoopMaxBytes is the most code runReg, the loop over the handler table,
// may compile to on amd64, with the state it takes and gives back inlined
// (800 bytes, 30 instructions of them the loop): a fixed loop the same for
// every operation, which an operation added does not grow
// (docs/register-interpreter.md, "The loop").
const regLoopMaxBytes = 864

// TestRegLoopIsSmall checks that runReg stays the small fixed loop it is:
// below regLoopMaxBytes of code, and storing nothing to the stack between
// one dispatch and the next -- what it keeps across a handler, the
// registers, the code and the state, is saved once, before the loop, and
// only read back after each call. A value live across the call that it
// stored at each dispatch would cost every instruction of every program.
// Where Go passes a call's arguments on the stack, as on 386, the stores
// of the handler's arguments to the bottom of the frame are the call's own,
// which every call makes there, and are not counted (stackArgBytes).
func TestRegLoopIsSmall(t *testing.T) { testRegLoopIsSmall(t, "reg_run.go", "runReg") }

// TestRegTypedLoopIsSmall is TestRegLoopIsSmall for runRegTyped, the
// typed table's copy of the loop (reg_typed.go).
func TestRegTypedLoopIsSmall(t *testing.T) { testRegLoopIsSmall(t, "reg_typed.go", "runRegTyped") }

// testRegLoopIsSmall checks the loop of the function name, in file.
func testRegLoopIsSmall(t *testing.T, file, name string) {
	if testing.Short() {
		t.Skip("builds and disassembles cmd/qjs")
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	// The loop's lines: from its dispatch to the state's read after it.
	first, last := 0, 0
	for i, l := range strings.Split(string(src), "\n") {
		switch strings.TrimSpace(l) {
		case "w = regAt(code, pc)":
			if first == 0 {
				first = i + 1
			}
		case "err := st.err":
			if first != 0 && last == 0 {
				last = i + 1
			}
		}
	}
	if first == 0 || last == 0 {
		t.Fatalf("no dispatch loop in %s", file)
	}
	exe := filepath.Join(t.TempDir(), "qjs.exe")
	if out, err := exec.Command(gotool, "build", "-o", exe, "../../cmd/qjs").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	nm, err := exec.Command(gotool, "tool", "nm", "-size", exe).Output()
	if err != nil {
		t.Fatalf("nm: %v", err)
	}
	size := -1
	sc := bufio.NewScanner(bytes.NewReader(nm))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 4 && f[2] == "T" && strings.HasSuffix(f[3], "/internal/vm.(*Runtime)."+name) {
			size, _ = strconv.Atoi(f[1])
		}
	}
	if size < 0 {
		t.Fatalf("no %s in the build", name)
	}
	if size > regLoopMaxBytes {
		t.Errorf("%s is %d bytes, past %d: the loop is the same for every operation, each of which is a handler (reg_table.go)", name, size, regLoopMaxBytes)
	}
	out, err := exec.Command(gotool, "tool", "objdump", "-s", `vm\.\(\*Runtime\)\.`+name+`$`, exe).CombinedOutput()
	if err != nil {
		t.Fatalf("objdump: %v\n%s", err, out)
	}
	args := stackArgBytes()
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasPrefix(f[0], file+":") {
			continue
		}
		line, err := strconv.Atoi(strings.TrimPrefix(f[0], file+":"))
		if err != nil || line < first || line >= last {
			continue
		}
		n++
		// An instruction of the loop that writes the stack: a move whose
		// last operand, its destination, is a slot of the frame.
		ops := strings.Split(strings.Join(f[3:], " "), ",")
		if dst := strings.TrimSpace(ops[len(ops)-1]); strings.HasPrefix(f[3], "MOV") && strings.Contains(dst, "(SP)") {
			if off, err := strconv.ParseInt(strings.TrimSuffix(dst, "(SP)"), 0, 64); err == nil && off >= 0 && off < args {
				continue
			}
			t.Errorf("%s's loop stores to the stack: %s", name, strings.TrimSpace(l))
		}
	}
	if n == 0 {
		t.Fatalf("no instruction of %s:%d-%d in %s", file, first, last, name)
	}
	t.Logf("%s: %d bytes, %d instructions in its loop", name, size, n)
}

// stackArgBytes is the bytes of a handler's arguments at the bottom of
// the caller's frame where Go's calls pass them on the stack: on every
// architecture without Go's register ABI, which has no flag of its own to
// ask. Where the ABI passes them in registers it is 0, and every store to
// the frame counts.
func stackArgBytes() int64 {
	switch runtime.GOARCH {
	case "amd64", "arm64", "loong64", "ppc64", "ppc64le", "riscv64":
		return 0
	}
	var n int64
	ft := reflect.TypeOf(regHandler(nil))
	for i := range ft.NumIn() {
		a := int64(ft.In(i).Align())
		n = (n+a-1)/a*a + int64(ft.In(i).Size())
	}
	return n
}
