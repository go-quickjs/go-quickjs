package fixedbase

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// build builds a small program for windows/amd64 into dir, whatever the
// host is.
func build(t *testing.T, dir, name string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go command")
	}
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte("package main\n\nfunc main() { println(\"hello\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", exe, src)
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return exe
}

// Go's linker gives a windows/amd64 image both flags; Clear takes them off,
// changes nothing else, and says the header's image base -- 0x140000000 in
// a build on Windows, which is PIE, 0x400000 in one cross-built elsewhere.
func TestClear(t *testing.T) {
	exe := build(t, t.TempDir(), "hello.exe")
	before, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	h, err := parse(before)
	if err != nil {
		t.Fatal(err)
	}
	if c := binary.LittleEndian.Uint16(before[h.chars:]); c&dynamicBase == 0 || c&highEntropyVA == 0 {
		t.Fatalf("the linker's DllCharacteristics are %#x: not both flags", c)
	}
	base, err := Clear(exe)
	if err != nil {
		t.Fatal(err)
	}
	if base != h.imageBase || base != 0x140000000 && base != 0x400000 {
		t.Fatalf("image base %#x, the header's %#x", base, h.imageBase)
	}
	after, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d bytes, were %d", len(after), len(before))
	}
	for i := range after {
		if after[i] != before[i] && i != h.chars {
			t.Fatalf("byte %#x changed", i)
		}
	}
	if c := binary.LittleEndian.Uint16(after[h.chars:]); c&(dynamicBase|highEntropyVA) != 0 {
		t.Fatalf("DllCharacteristics %#x after", c)
	}
	// Again: nothing to clear.
	if again, err := Clear(exe); err != nil || again != base {
		t.Fatalf("again: %#x, %v", again, err)
	}
	if _, err := parse([]byte("MZ but nothing more of a PE file, sixty-four bytes or so of it...")); err == nil {
		t.Fatal("parsed what has no PE signature")
	}
}

// Two copies of one build, at two paths, load where Windows chooses; with
// the flags cleared each loads at its image base.
func TestFixLoadsAtItsBase(t *testing.T) {
	if !Default {
		t.Skip("loads Windows amd64 programs")
	}
	dir := t.TempDir()
	exe := build(t, dir, "hello.exe")
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "another-name.exe")
	if err := os.WriteFile(other, b, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{exe, other} {
		got, err := LoadedBase(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s, as built: loaded at %#x", filepath.Base(p), got)
		base, err := Fix(p)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := LoadedBase(p); err != nil || got != base {
			t.Fatalf("%s fixed: loaded at %#x (%v), its base %#x", p, got, err, base)
		}
	}
}

// A fresh copy is a file of its own beside the build, the same bytes, gone
// once removed.
func TestFreshCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vb-base-0.exe")
	if err := os.WriteFile(path, []byte("MZ and the rest"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, remove, err := FreshCopy(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(c) != dir || c == path || filepath.Ext(c) != ".exe" {
		t.Fatalf("copy %s", c)
	}
	if b, err := os.ReadFile(c); err != nil || string(b) != "MZ and the rest" {
		t.Fatalf("copy holds %q (%v)", b, err)
	}
	c2, remove2, err := FreshCopy(path)
	if err != nil || c2 == c {
		t.Fatalf("second copy %s (%v)", c2, err)
	}
	remove()
	remove2()
	if _, err := os.Stat(c); !os.IsNotExist(err) {
		t.Fatalf("copy left: %v", err)
	}
}
