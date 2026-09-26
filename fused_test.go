package quickjs_test

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// fusedInstruction is an arm64 fused multiply-add in the compiler's
// assembly listing, with the source line it came from.
var fusedInstruction = regexp.MustCompile(`\(([^()]+\.go):(\d+)\)\s+(FN?M(?:ADD|SUB)[SD])\b`)

// Go may fuse x*y + z into one instruction on arm64, rounding once where
// every other platform rounds twice, and so answer differently there:
// parseInt and a number literal in a radix, Number.prototype.toString in
// one, and Math.hypot could differ on a Mac, and Date.UTC did, in go-intl.
// Every product that feeds a sum is rounded by an explicit conversion
// instead. This compiles the module for arm64 and fails on any fused
// instruction a line does not ask for by calling math.FMA, so that a machine
// that never fuses still catches one.
func TestNoFusedArithmetic(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the module for arm64")
	}
	cmd := exec.Command("go", "build", "-gcflags=github.com/go-quickjs/go-quickjs/...=-S", "./...")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build for arm64: %v\n%s", err, out)
	}
	sources := map[string][]string{}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		m := fusedInstruction.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		file, line := m[1], m[2]
		if _, ok := sources[file]; !ok {
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			sources[file] = strings.Split(string(b), "\n")
		}
		n, _ := strconv.Atoi(line)
		if n >= 1 && n <= len(sources[file]) && strings.Contains(sources[file][n-1], "math.FMA(") {
			continue
		}
		t.Errorf("%s:%s: %s: round the product with an explicit float64 conversion", file, line, m[3])
	}
}
