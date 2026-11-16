package vm

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
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
func TestRegLoopIsSmall(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and disassembles cmd/qjs")
	}
	gotool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command")
	}
	src, err := os.ReadFile("zzreg_run.go")
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
		t.Fatal("no dispatch loop in zzreg_run.go")
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
		if len(f) == 4 && f[2] == "T" && strings.HasSuffix(f[3], "/internal/vm.(*Runtime).runReg") {
			size, _ = strconv.Atoi(f[1])
		}
	}
	if size < 0 {
		t.Fatal("no runReg in the build")
	}
	if size > regLoopMaxBytes {
		t.Errorf("runReg is %d bytes, past %d: the loop is the same for every operation, each of which is a handler (zzreg_table.go)", size, regLoopMaxBytes)
	}
	out, err := exec.Command(gotool, "tool", "objdump", "-s", `vm\.\(\*Runtime\)\.runReg$`, exe).CombinedOutput()
	if err != nil {
		t.Fatalf("objdump: %v\n%s", err, out)
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 4 || !strings.HasPrefix(f[0], "zzreg_run.go:") {
			continue
		}
		line, err := strconv.Atoi(strings.TrimPrefix(f[0], "zzreg_run.go:"))
		if err != nil || line < first || line >= last {
			continue
		}
		n++
		// An instruction of the loop that writes the stack: a move whose
		// last operand, its destination, is a slot of the frame.
		ops := strings.Split(strings.Join(f[3:], " "), ",")
		if strings.HasPrefix(f[3], "MOV") && strings.Contains(ops[len(ops)-1], "(SP)") {
			t.Errorf("runReg's loop stores to the stack: %s", strings.TrimSpace(l))
		}
	}
	if n == 0 {
		t.Fatalf("no instruction of zzreg_run.go:%d-%d in runReg", first, last)
	}
	t.Logf("runReg: %d bytes, %d instructions in its loop", size, n)
}
