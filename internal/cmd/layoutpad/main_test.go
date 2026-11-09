package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// go tool nm's names may have spaces: a generic's shape is a struct.
func TestParseNM(t *testing.T) {
	out := []byte("  140261880        224 T github.com/x/vm.joinSize\n" +
		"  140454b20        224 T github.com/x/vm.strAppendNode[go.shape.struct {}].func1\n" +
		"  1420c2a20         16 B runtime.writeBarrier\n")
	syms := parseNM(out)
	s, ok := syms.byName["github.com/x/vm.strAppendNode[go.shape.struct {}].func1"]
	if !ok || s.addr != 0x140454b20 || s.size != 224 || s.kind != "T" {
		t.Fatalf("parsed %+v (%v)", s, ok)
	}
	if w := syms.byName["runtime.writeBarrier"]; w.addr%dataMod != 0x20 || w.kind != "B" {
		t.Fatalf("writeBarrier %+v", w)
	}
}

// A targets file is written back as it was read, its values changed.
func TestTargetsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.txt")
	src := "# comment\ntoolchain go1.27.3\n\nconfig windows/amd64 plain\n" +
		"anchor V  0x00580 github.com/x/vm.(*Runtime).executeAt\n" +
		"anchor CZ 0x00b60 github.com/x/vm.strAppendNode[go.shape.struct {}].func1\n" +
		"anchor DW 0x020 runtime.writeBarrier\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tg, err := readTargets(path)
	if err != nil {
		t.Fatal(err)
	}
	c := tg.configs[0]
	if c.goos != "windows" || c.jit || len(c.anchors) != 3 || c.anchors[1].symbol != "github.com/x/vm.strAppendNode[go.shape.struct {}].func1" {
		t.Fatalf("read %+v", c)
	}
	c.anchors[0].target = 0x5c0
	if err := writeTargets(path, tg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if want := strings.Replace(src, "0x00580", "0x005c0", 1); string(b) != want {
		t.Fatalf("wrote\n%s\nwant\n%s", b, want)
	}
}

// A pad's stores: as many as the constant says, each to its own byte, none
// next to another.
func TestStores(t *testing.T) {
	var b strings.Builder
	stores(&b, "\t", "n")
	src := b.String()
	if got := strings.Count(src, "layoutPadBytes["); got != maxStores {
		t.Fatalf("%d stores, want %d", got, maxStores)
	}
	if strings.Contains(src, "layoutPadBytes[1]") || !strings.Contains(src, "layoutPadBytes[2044]") || strings.Contains(src, "layoutPadBytes[2046]") {
		t.Fatal("stores are not every other byte of the array")
	}
	if got := strings.Count(src, "if max(n, 0)&(1<<"); got != storeBits {
		t.Fatalf("%d groups, want %d", got, storeBits)
	}
}

// A checkout on Windows may give go.mod CRLF line endings.
func TestIsModule(t *testing.T) {
	for _, c := range []struct {
		gomod string
		want  bool
	}{
		{"module github.com/go-quickjs/go-quickjs\n\ngo 1.24.0\n", true},
		{"module github.com/go-quickjs/go-quickjs\r\n\r\ngo 1.24.0\r\n", true},
		{"// comment\nmodule github.com/go-quickjs/go-intl\n", false},
		{"go 1.24.0\n", false},
	} {
		if got := isModule([]byte(c.gomod)); got != c.want {
			t.Errorf("isModule(%q) = %v", c.gomod, got)
		}
	}
}
