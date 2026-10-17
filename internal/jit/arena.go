package jit

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"unsafe"
)

// An Arena holds a runtime's native code in a few large mappings, rather
// than one mapping per function: a process with many runtimes, each with
// its cache of compiled functions, would otherwise run out of mappings
// (Linux's vm.max_map_count) and, on Windows, of address space, which
// VirtualAlloc hands out 64 KiB at a time.
//
// Functions are packed into chunks of chunkBytes. A chunk's pages are
// executable and never writable at once: placing a function makes the pages
// it touches writable, copies it and seals them again. That is sound only
// while no code in those pages runs, and a runtime's native code never does
// while Go runs it: native code returns to Go before Go does anything, and
// an arena belongs to one runtime, which is not used concurrently. A chunk
// is unmapped when the last function in it is released.
//
// Code is released by its owner, or by its finalizer on another goroutine,
// so the arena locks.
type Arena struct {
	mu     sync.Mutex
	chunks []*chunk
	// mapped and placed count the chunks mapped and the functions placed,
	// which choose LayoutSeed's offsets.
	mapped, placed uint64
}

type chunk struct {
	mem  []byte
	used int // bytes handed out, from the start
	live int // bytes still owned
}

const (
	// chunkBytes is a chunk's size: Windows' allocation granularity, and a
	// whole number of pages everywhere. A larger function gets a chunk of
	// its own, rounded up to a multiple of it.
	chunkBytes = 64 << 10
	// codeAlign aligns each function to a cache line, and to the 64 bytes
	// arm64's instruction cache maintenance works in.
	codeAlign = 64
)

// NewArena returns an empty arena; it maps nothing until code is placed.
func NewArena() *Arena { return &Arena{} }

// FirstOffset is where in its first chunk an arena places its first code,
// codeAlign-rounded: 0 but for a benchmark, which varies it (v8bench's
// V8BENCH_JITPAD) because where native code is decides how fast it runs.
// A program whose hot code is larger than a few kilobytes spreads it over
// the op cache's sets (64 of 8 ways on AMD's Zen 2, by address bits 6 to
// 11) as it happens to lie: AWFY's Towers, its 60 KB of code all native,
// had 81% of its ops from the op cache and an IPC of 3.5 at some offsets,
// 43% and 2.8 at others (AMDuProfPcm), the same instructions retired at
// the same rate of mispredictions, and ran 20% faster. A comparison of two
// builds that placed each at one offset compared those offsets as much as
// the builds.
var FirstOffset int

// LayoutSeed, when not 0, has an arena vary where it places code past its
// first offset, as a benchmark does (v8bench's V8BENCH_JITSEED): each
// chunk after the first begins at an offset under 4 KiB, and a gap of 0 to
// 3 cache lines comes before each function placed after a chunk's first,
// each as the seed and the chunk's or the function's number choose.
// FirstOffset moves only what goes in the first chunk: code compiled
// later -- a benchmark's late in a suite, past a few megabytes of others'
// -- lands where the code before it ends, at the same place for every
// FirstOffset, and its op-cache sets were the same in every placement of
// a comparison. An offset under 4 KiB moves a chunk's code across all of
// the op cache's sets (address bits 6 to 11), and the gaps move functions
// against each other. 0 packs code as it comes.
var LayoutSeed uint64

// layoutDraw is LayoutSeed's choice for the k-th thing of a kind (salt):
// splitmix64 of the seed, the salt and k.
func layoutDraw(salt, k uint64) uint64 {
	x := LayoutSeed ^ salt*0x9e3779b97f4a7c15 ^ k*0xbf58476d1ce4e5b9
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// layoutChunkSpan and layoutGapLines bound LayoutSeed's offsets: a chunk's
// first code within 4 KiB of its start, a function within 3 cache lines of
// the one before it.
const (
	layoutChunkSpan = 4 << 10
	layoutGapLines  = 4
)

// Mapped reports how many bytes of code the arena has mapped, and how many
// of them functions own: the rest is space their release left in chunks
// that others still hold.
func (a *Arena) Mapped() (mapped, live int) {
	if a == nil {
		return 0, 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, c := range a.chunks {
		mapped += len(c.mem)
		live += c.live
	}
	return mapped, live
}

// Chunks reports how many mappings the arena holds.
func (a *Arena) Chunks() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.chunks)
}

func alignUp(n, to int) int { return (n + to - 1) / to * to }

// place copies instructions into the arena and seals them executable. The
// slice it returns is the code's whole allocation, codeAlign-rounded, which
// release takes back.
func (a *Arena) place(instructions []byte) ([]byte, error) {
	if len(instructions) == 0 || len(instructions) > MaxCodeBytes {
		return nil, fmt.Errorf("invalid native kernel size: %d", len(instructions))
	}
	if err := executablePolicy(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	n := alignUp(len(instructions), codeAlign)
	gap := 0
	if LayoutSeed != 0 {
		a.placed++
		gap = int(layoutDraw(1, a.placed)%layoutGapLines) * codeAlign
	}
	var c *chunk
	if k := len(a.chunks); k > 0 && a.chunks[k-1].used+gap+n <= len(a.chunks[k-1].mem) {
		c = a.chunks[k-1]
	} else {
		mem, err := mapCode(alignUp(n, chunkBytes))
		if err != nil {
			return nil, fmt.Errorf("allocate code: %w", err)
		}
		c = &chunk{mem: mem}
		gap = 0
		if pad := alignUp(FirstOffset, codeAlign); len(a.chunks) == 0 && pad > 0 && pad+n <= len(mem) {
			c.used = pad
		} else if LayoutSeed != 0 && len(a.chunks) > 0 {
			a.mapped++
			if pad := int(layoutDraw(2, a.mapped)%layoutChunkSpan) / codeAlign * codeAlign; pad+n <= len(mem) {
				c.used = pad
			}
		}
		a.chunks = append(a.chunks, c)
	}
	start := c.used + gap
	page := os.Getpagesize()
	pages := c.mem[start/page*page : min(alignUp(start+n, page), len(c.mem))]
	code := c.mem[start : start+n : start+n]
	if err := a.write(c, pages, code, instructions); err != nil {
		if c.live == 0 {
			// A chunk mapped for this code, or emptied before it, holds
			// nothing: give it back.
			err = errors.Join(err, a.unmap(c))
		}
		return nil, err
	}
	c.used = start + n
	c.live += n
	return code, nil
}

// write copies instructions into code, through its pages made writable for
// the copy, then seals the pages and publishes them to instruction fetch.
func (a *Arena) write(c *chunk, pages, code, instructions []byte) error {
	if err := protectCode(pages, false); err != nil {
		return fmt.Errorf("open code: %w", err)
	}
	copy(code, instructions)
	if err := protectCode(pages, true); err != nil {
		return fmt.Errorf("seal code: %w", err)
	}
	if err := flushCode(code); err != nil {
		return fmt.Errorf("flush instruction cache: %w", err)
	}
	return nil
}

// unmap gives c back to the OS and drops it.
func (a *Arena) unmap(c *chunk) error {
	if err := unmapCode(c.mem); err != nil {
		return err
	}
	for i := range a.chunks {
		if a.chunks[i] == c {
			a.chunks = append(a.chunks[:i], a.chunks[i+1:]...)
			break
		}
	}
	return nil
}

// release gives code back. Its chunk is unmapped once nothing in it is
// owned; if the OS refuses, the code stays owned, for a retry.
func (a *Arena) release(code []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	at := uintptr(unsafe.Pointer(unsafe.SliceData(code)))
	for _, c := range a.chunks {
		base := uintptr(unsafe.Pointer(unsafe.SliceData(c.mem)))
		if at < base || at >= base+uintptr(len(c.mem)) {
			continue
		}
		if c.live > len(code) {
			c.live -= len(code)
			return nil
		}
		if err := a.unmap(c); err != nil {
			return err
		}
		c.live = 0
		return nil
	}
	return errors.New("jit: code is not in its arena")
}
