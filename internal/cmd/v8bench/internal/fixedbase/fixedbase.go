// Package fixedbase makes a Windows build of the benchmarks load at its link
// address, as a Linux build does, so that every copy of it times alike.
//
// Windows loads an image whose PE header has DYNAMICBASE where it chooses,
// once for each file until the machine restarts, in steps of 64 KB, and with
// HIGH_ENTROPY_VA anywhere in the 64-bit address space. The layout lock and
// the placements hold package vm's code modulo 4 KB, which those steps keep,
// but Zen 2 tells the higher bits apart too: two copies of one build of
// 91f885a1 took 26 and 46 ns a run of json_stringify_ints, and ten copies of
// twelve 49 ns a run of json_stringify_decimals where the other two took 35.
// A copy keeps its speed from run to run, so a comparison of two builds was
// also one of where Windows put each. With both flags cleared the loader
// puts the image at its preferred base, which Go's linker sets
// (0x140000000 on amd64), the same for every copy: what a Linux build,
// linked to run at one address, always does.
//
// That is not all of it: a copy at its image base can still keep a speed of
// its own -- one of eight copies of b5df63ea's took 48 ns a run of
// json_stringify_decimals where the others took 35, every run, and the same
// bytes copied to its path again took 35 -- most likely for where the
// file's pages lie in memory, which the caches past the first index by.
// FreshCopy gives each run a copy of its own, so that a run's speed is not
// its file's: the tools time each run from one.
package fixedbase

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Default says whether the benchmarks' tools clear the flags unless told
// not to: on windows/amd64, where Go builds such images and the loader
// honours a fixed base. Windows on arm64 requires a relocatable image.
const Default = runtime.GOOS == "windows" && runtime.GOARCH == "amd64"

// The PE header's DllCharacteristics bits that let the loader move an image.
const (
	dynamicBase   = 0x0040 // IMAGE_DLLCHARACTERISTICS_DYNAMIC_BASE
	highEntropyVA = 0x0020 // IMAGE_DLLCHARACTERISTICS_HIGH_ENTROPY_VA
)

// header is where a PE file's DllCharacteristics and ImageBase are.
type header struct {
	chars     int // the DllCharacteristics field's offset
	imageBase uint64
}

// parse finds the fields in a PE file's bytes: the DOS header's e_lfanew at
// 0x3c, the PE signature there, the COFF header's 20 bytes, then the
// optional header, PE32 (magic 0x10b, a 4-byte ImageBase at 28) or PE32+
// (0x20b, an 8-byte one at 24), its DllCharacteristics at 70 in both.
func parse(b []byte) (header, error) {
	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return header{}, errors.New("not a PE file: no MZ header")
	}
	pe := int(binary.LittleEndian.Uint32(b[0x3c:]))
	opt := pe + 4 + 20
	if pe < 0 || opt+72 > len(b) || string(b[pe:pe+4]) != "PE\x00\x00" {
		return header{}, errors.New("not a PE file: no PE signature")
	}
	h := header{chars: opt + 70}
	switch magic := binary.LittleEndian.Uint16(b[opt:]); magic {
	case 0x20b:
		h.imageBase = binary.LittleEndian.Uint64(b[opt+24:])
	case 0x10b:
		h.imageBase = uint64(binary.LittleEndian.Uint32(b[opt+28:]))
	default:
		return header{}, fmt.Errorf("PE optional header of magic %#x", magic)
	}
	return h, nil
}

// Clear clears DYNAMICBASE and HIGH_ENTROPY_VA in the PE file at path and
// says the image base it then loads at. A file that has neither is left as
// it is.
func Clear(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	h, err := parse(b)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	c := binary.LittleEndian.Uint16(b[h.chars:])
	if c&(dynamicBase|highEntropyVA) == 0 {
		return h.imageBase, nil
	}
	binary.LittleEndian.PutUint16(b[h.chars:], c&^(dynamicBase|highEntropyVA))
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return h.imageBase, os.WriteFile(path, b, fi.Mode())
}

// FreshCopy copies the build at path to a file of its own beside it, for
// one run, and gives the copy and what removes it.
func FreshCopy(path string) (string, func(), error) {
	in, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer in.Close()
	ext := filepath.Ext(path)
	out, err := os.CreateTemp(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ext)+"-run-*"+ext)
	if err != nil {
		return "", nil, err
	}
	name := out.Name()
	remove := func() { os.Remove(name) }
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o755)
	}
	if err != nil {
		remove()
		return "", nil, err
	}
	return name, remove, nil
}

// Fix clears the flags in the build at path and checks that Windows then
// loads it at its image base, which it returns.
func Fix(path string) (uint64, error) {
	base, err := Clear(path)
	if err != nil {
		return 0, err
	}
	got, err := LoadedBase(path)
	if err != nil {
		return 0, fmt.Errorf("%s: where it loads: %w", path, err)
	}
	if got != base {
		return 0, fmt.Errorf("%s loads at %#x, not at its image base %#x", path, got, base)
	}
	return base, nil
}
