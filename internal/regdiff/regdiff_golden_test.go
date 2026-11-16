package regdiff

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// The differential tests' recorded answers. Each comparison's program and
// calls were run on the stack interpreter and the tree tier, the tiers the
// register interpreter replaced, and every answer recorded in
// testdata/golden.json, keyed by the test's name and the comparison's
// place among its test's, with a hash of its program and calls. A call
// whose two runs there answered differently -- a clock, Math.random -- is
// recorded as null and not compared. A case changed since is to have its
// recorded answers changed with it, by hand: the tiers that gave them are
// gone.

// goldenPath is the recorded answers' file.
const goldenPath = "testdata/golden.json"

// goldenCase is one comparison's recorded answers: a hash of its program
// and calls, which says the case is still the one recorded, and each
// call's answer, an answer longer than goldenLong by its hash.
type goldenCase struct {
	Program string    `json:"program"`
	Answers []*string `json:"answers"`
}

// goldenLong is the length past which an answer is recorded by its hash.
const goldenLong = 160

var (
	goldenOnce  sync.Once
	goldenCases map[string]goldenCase
	goldenErr   error
	goldenMu    sync.Mutex
	goldenSeq   = map[string]int{}
)

// goldenKey is the key of t's next comparison.
func goldenKey(t *testing.T) string {
	goldenMu.Lock()
	defer goldenMu.Unlock()
	n := goldenSeq[t.Name()]
	goldenSeq[t.Name()] = n + 1
	return fmt.Sprintf("%s#%d", t.Name(), n)
}

// goldenProgram is the hash of a comparison's program and calls, its
// source's line ends LF's: a checkout that writes CRLF, as Windows' Git
// does by default, has the same program, which reads the files the
// comparisons run (testdata's scripts, the micro-benchmarks) with them.
func goldenProgram(src string, calls []string) string {
	h := sha256.New()
	h.Write([]byte(strings.ReplaceAll(src, "\r\n", "\n")))
	for _, c := range calls {
		h.Write([]byte{0})
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}

// goldenAnswer is answer as it is recorded.
func goldenAnswer(answer string) string {
	if len(answer) <= goldenLong {
		return answer
	}
	sum := sha256.Sum256([]byte(answer))
	return fmt.Sprintf("sha256:%s len %d", hex.EncodeToString(sum[:16]), len(answer))
}

// goldenCheck compares the answers on with those recorded for key, and
// fails where they differ. It returns how many calls it skipped, whose
// recorded answers varied.
func goldenCheck(t *testing.T, key, src string, calls, on []string) (skipped int) {
	t.Helper()
	goldenOnce.Do(func() {
		b, err := os.ReadFile(goldenPath)
		if err == nil {
			err = json.Unmarshal(b, &goldenCases)
		}
		goldenErr = err
	})
	if goldenErr != nil {
		t.Fatalf("%s: %v", goldenPath, goldenErr)
	}
	c, ok := goldenCases[key]
	if !ok {
		t.Fatalf("%s has no answers for %s", goldenPath, key)
	}
	if c.Program != goldenProgram(src, calls) || len(c.Answers) != len(calls) {
		t.Fatalf("%s: the case is not the one recorded in %s; its recorded answers are to be changed with it", key, goldenPath)
	}
	bad := 0
	for i, want := range c.Answers {
		if want == nil {
			skipped++
			continue
		}
		if got := goldenAnswer(on[i]); got != *want {
			if bad++; bad <= 20 {
				t.Errorf("%s:\n  recorded: %.300s\n  register: %.300s", calls[i], *want, got)
			}
		}
	}
	if bad > 20 {
		t.Errorf("and %d more", bad-20)
	}
	return skipped
}
