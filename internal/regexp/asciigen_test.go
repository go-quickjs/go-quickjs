package regexp

import (
	"bytes"
	"os"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/regexp/internal/asciigen"
)

// TestASCIIMatcherGenerated pins that exec_ascii.go and exec_utf8.go are what
// go generate makes of exec.go: the matchers over bytes kept the same as the
// one over code units.
func TestASCIIMatcherGenerated(t *testing.T) {
	for file, v := range map[string]asciigen.Variant{
		"exec_ascii.go": asciigen.ASCII,
		"exec_utf8.go":  asciigen.UTF8,
	} {
		want, err := asciigen.Generate("exec.go", v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is out of date: run go generate ./internal/regexp", file)
		}
	}
}
