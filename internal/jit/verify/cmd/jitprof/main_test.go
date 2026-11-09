package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestParseSamples(t *testing.T) {
	got := parseSamples(`Samples:
samples/count cpu/nanoseconds
  7 70000000: 1 3
  5 50000000: 2
  2 20000000: 3
Locations
  1: 0x130000002 :0:0 s=0
  2: 0x100145003 M=1 runtime._ExternalCode /usr/local/go/src/runtime/proc.go:5788:0 s=5788
  3: 0x100123456 M=1 main.main /tmp/main.go:12:0 s=10
Mappings
`)
	want := []sample{
		{count: 7, addr: 0x130000002, unknown: true},
		{count: 5, addr: 0x100145003, external: true},
		{count: 2, addr: 0x100123456},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("samples %v, want %v", got, want)
	}
}

func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		arch string
		code []byte
		text int
		want []inst
	}{
		{"", []byte{0x48, 0x89, 0xD8, 0xC3, 0}, 4, []inst{{0, 3, "mov rax, rbx"}, {3, 1, "ret"}}},
		{"arm64", []byte{0xE0, 0x03, 0x01, 0xAA, 0xC0, 0x03, 0x5F, 0xD6, 0}, 8, []inst{{0, 4, "mov x0, x1"}, {4, 4, "ret"}}},
		{"arm64", []byte{0xC0, 0x03, 0x5F}, 3, []inst{{off: 0}}},
		{"arm64", []byte{0xFF, 0xFF, 0xFF, 0xFF}, 4, []inst{{off: 0}}},
	} {
		c := &code{Arch: tc.arch, Bytes: tc.code, Text: tc.text}
		got := decode(c)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v, want %v", tc.arch, got, tc.want)
		}
		for i, want := range tc.want {
			if got[i] != want {
				t.Fatalf("%s instruction %d: %v, want %v", tc.arch, i, got[i], want)
			}
			for off := want.off; off < want.off+max(1, want.size); off++ {
				in, ok := instAt(got, off)
				if ok != (want.size > 0) || ok && in != want {
					t.Fatalf("%s offset %d: %v, %v; want %v", tc.arch, off, in, ok, want)
				}
			}
		}
		if _, ok := instAt(got, -1); ok {
			t.Fatal("found an instruction before the code")
		}
		if _, ok := instAt(got, tc.text); ok {
			t.Fatal("found an instruction past the code")
		}
	}
}

func TestReadARM64Code(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code.jsonl")
	want := &code{Arch: "arm64", Addr: 0x10000, Text: 4, Bytes: []byte{0xC0, 0x03, 0x5F, 0xD6}}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	cs, err := readCodes(path)
	if err != nil || len(cs) != 1 || cs[0].Arch != "arm64" {
		t.Fatalf("codes %v, error %v", cs, err)
	}
	c := find(cs, want.Addr+2)
	if c == nil || c.tagAt(2) != "unannotated" || len(decode(c)) != 1 {
		t.Fatalf("arm64 sample is not mapped to its instruction: %v", c)
	}
}
