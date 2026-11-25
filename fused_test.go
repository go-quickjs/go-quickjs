package quickjs_test

import (
	"bufio"
	"io"
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

// listingLine is a line of the compiler's assembly listing: an
// instruction or a data word, indented, or a symbol's header: its name,
// which may have spaces, its kind and its size.
var listingLine = regexp.MustCompile(`^(?:\s|.* S[A-Z]+\b.* size=\d)`)

// Go may fuse x*y + z into one instruction on arm64, rounding once where
// every other platform rounds twice, and so answer differently there:
// parseInt and a number literal in a radix, Number.prototype.toString in
// one, and Math.hypot could differ on a Mac, and Date.UTC did, in go-intl.
// Every product that feeds a sum is rounded by an explicit conversion
// instead. This compiles the module for arm64 and fails on any fused
// instruction a line does not ask for by calling math.FMA, so that a machine
// that never fuses still catches one.
//
// The listing is about half a gigabyte. It is scanned as the compiler
// writes it, through a pipe, rather than held, and of it only what is not
// the listing -- a compile error, under its package's header -- is kept,
// for a failed build's message.
func TestNoFusedArithmetic(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("compiles the module for arm64")
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command("go", "build", "-gcflags=github.com/go-quickjs/go-quickjs/...=-S", "./...")
	cmd.Env = append(os.Environ(), "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		t.Fatalf("go build for arm64: %v", err)
	}
	type finding struct{ file, line, op string }
	var found []finding
	var others strings.Builder
	header := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		text := sc.Text()
		if m := fusedInstruction.FindStringSubmatch(text); m != nil {
			found = append(found, finding{m[1], m[2], m[3]})
		} else if strings.HasPrefix(text, "# ") {
			header = text
		} else if !listingLine.MatchString(text) && others.Len() < 1<<20 {
			if header != "" {
				others.WriteString(header)
				others.WriteByte('\n')
				header = ""
			}
			others.WriteString(text)
			others.WriteByte('\n')
		}
	}
	scanErr := sc.Err()
	// What a scan that stopped early leaves, so that the build is not held
	// up writing to a full pipe.
	io.Copy(io.Discard, r)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("go build for arm64: %v\n%s", err, others.String())
	}
	if scanErr != nil {
		t.Fatalf("reading the listing: %v", scanErr)
	}
	sources := map[string][]string{}
	for _, f := range found {
		if _, ok := sources[f.file]; !ok {
			b, err := os.ReadFile(f.file)
			if err != nil {
				t.Fatal(err)
			}
			sources[f.file] = strings.Split(string(b), "\n")
		}
		n, _ := strconv.Atoi(f.line)
		if n >= 1 && n <= len(sources[f.file]) && strings.Contains(sources[f.file][n-1], "math.FMA(") {
			continue
		}
		t.Errorf("%s:%s: %s: round the product with an explicit float64 conversion", f.file, f.line, f.op)
	}
}
