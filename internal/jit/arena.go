package jit

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"unsafe"
)

// An Arena holds a runtime's native code in a few large mappings, rather
// than one mapping per function: a process with many runtimes, each with
// its cache of compiled functions, would otherwise run out of mappings
// (Linux's vm.max_map_count) and, on Windows, of address space, which
// VirtualAlloc hands out 64 KiB at a time.
//
// Functions are packed into chunks of chunkBytes. A function longer than a
// chunk has a mapping of its own, as long as it needs, a whole number of
// chunks, which no other code shares: shared, it had stayed mapped after
// the function was released for the few kilobytes of others' placed after
// it, megabytes no budget counts. Code of any length is placed so: its
// branches reach across all of it (amd64's rel32 2 GB either way; arm64's
// assembler keeps its own in reach, internal/jit/asm/arm64's pools). A
// chunk's pages are
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
	// open is the chunk code is packed into, the last mapped that is not a
	// function's own, or nil.
	open *chunk
	// mapped and placed count the chunks mapped and the functions placed,
	// which choose LayoutSeed's offsets.
	mapped, placed uint64
	// reuse says code is placed in the space released code left in a
	// chunk, as well as past what the chunk handed out (NewReusingArena).
	reuse bool
}

type chunk struct {
	mem  []byte
	used int // bytes handed out, from the start
	live int // bytes still owned
	// own says the chunk is one function's, longer than chunkBytes.
	own bool
	// free is the space released code left below used, in a reusing
	// arena: by offset, none adjacent to another.
	free []span
}

// span is a run of a chunk's bytes, from off.
type span struct{ off, n int }

const (
	// chunkBytes is a chunk's size: Windows' allocation granularity, and a
	// whole number of pages everywhere. A larger function gets a mapping
	// of its own, rounded up to a multiple of it.
	chunkBytes = 64 << 10
	// codeAlign aligns each function to a cache line, and to the 64 bytes
	// arm64's instruction cache maintenance works in.
	codeAlign = 64
)

// NewArena returns an empty arena; it maps nothing until code is placed.
func NewArena() *Arena { return &Arena{} }

// NewReusingArena returns an empty arena that places code in the space
// that code released before it left in a chunk, the first such space
// large enough, or past the code of any chunk with room, before it maps a
// new chunk: for an owner that drops some of its code while the rest runs
// on, whose chunks would otherwise stay mapped, and grow in number, while
// any code in them lives. Placing there makes pages that hold other code
// writable, as placing past a chunk's code can, under the same rule: none
// of the owner's code runs then. It places each code at the start of its
// cache line and next to the code before it, whatever FirstOffset,
// LayoutSeed and LineOffset say, which are benchmarks' knobs for the other
// arenas.
func NewReusingArena() *Arena { return &Arena{reuse: true} }

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

// LineOffset, when not 0, has an arena place each function's code that
// many bytes into its cache line rather than at the line's start, the
// bytes before it unused and a line more taken: a benchmark's knob
// (v8bench's V8BENCH_JITLINE), as FirstOffset and LayoutSeed are. Those
// move code by whole lines, and a loop's header starts a line within its
// function (mir's loop alignment), so without it every placement has the
// same code at the same offsets within its lines -- the same 32-byte
// fetch windows, the same branches and fused pairs across line ends --
// and a change that moves code within its lines is judged at one of the
// 64 offsets. Taken mod codeAlign, and on arm64 rounded down to its
// instructions' 4 bytes. The code is the same at every offset, its jumps
// and its constants relative to it, and so is its slice (Text).
var LineOffset int

// lineShift is LineOffset as place applies it.
func lineShift() int {
	s := LineOffset % codeAlign
	if s < 0 {
		s += codeAlign
	}
	if runtime.GOARCH == "arm64" {
		s &^= 3
	}
	return s
}

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
// release takes back; with a LineOffset, as long, from that offset into
// an allocation a line longer.
func (a *Arena) place(instructions []byte) ([]byte, error) {
	if len(instructions) == 0 {
		return nil, fmt.Errorf("invalid native kernel size: %d", len(instructions))
	}
	if err := executablePolicy(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reuse {
		return a.placeReusing(instructions)
	}
	shift := 0
	if LineOffset != 0 {
		shift = lineShift()
	}
	// The code's slice is its instructions codeAlign-rounded at every
	// offset, so that what a profiler reads of it (Text) is the same.
	size := alignUp(len(instructions), codeAlign)
	n := alignUp(shift+size, codeAlign)
	gap := 0
	if LayoutSeed != 0 {
		a.placed++
		gap = int(layoutDraw(1, a.placed)%layoutGapLines) * codeAlign
	}
	var c *chunk
	if o := a.open; o != nil && o.used+gap+n <= len(o.mem) {
		c = o
	} else {
		mem, err := mapCode(alignUp(n, chunkBytes))
		if err != nil {
			return nil, fmt.Errorf("allocate code: %w", err)
		}
		c = &chunk{mem: mem, own: len(mem) > chunkBytes}
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
		if !c.own {
			a.open = c
		}
	}
	start := c.used + gap
	page := os.Getpagesize()
	pages := c.mem[start/page*page : min(alignUp(start+n, page), len(c.mem))]
	code := c.mem[start+shift : start+shift+size : start+shift+size]
	if err := a.write(c, pages, code, instructions); err != nil {
		if c.live == 0 {
			// A chunk mapped for this code, or emptied before it, holds
			// nothing: give it back.
			err = errors.Join(err, a.unmap(c))
		}
		return nil, err
	}
	c.used = start + n
	c.live += len(code)
	return code, nil
}

// placeApart copies each of list into the arena, as place does, on pages
// that hold no code placed before them and were never executable: a
// batch's first code starts at a page boundary past everything placed in
// the chunk code is packed into, or in a new chunk, and the batch's later
// codes follow it; code longer than a chunk has one of its own, as place
// gives it. Code of the arena may run on other threads meanwhile, since none of
// it is in the pages written, and no core can hold instructions fetched
// from them before they are sealed. Code placed later never shares those
// pages while they are written, the arena being locked; place may later
// share the batch's last page, under its own rule. The slices it returns
// are as place's; on an error none is placed. A reusing arena keeps the
// space between its chunk's code and the batch's first page for later
// code, and ignores FirstOffset and LineOffset, as its place does.
func (a *Arena) placeApart(list [][]byte) ([][]byte, error) {
	for _, instructions := range list {
		if len(instructions) == 0 {
			return nil, fmt.Errorf("invalid native kernel size: %d", len(instructions))
		}
	}
	if err := executablePolicy(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	shift := 0
	if LineOffset != 0 && !a.reuse {
		shift = lineShift()
	}
	page := os.Getpagesize()
	// A segment is the run of the batch's codes in one chunk, written
	// through its pages at once.
	type segment struct {
		c        *chunk
		from, to int
		first    int
	}
	var segs []segment
	out := make([][]byte, len(list))
	undo := func() error {
		var errs []error
		for i, code := range out {
			if code == nil {
				continue
			}
			c := a.chunkOf(code)
			c.live -= len(code)
			if c.live == 0 {
				errs = append(errs, a.unmap(c))
			} else if a.reuse {
				base := uintptr(unsafe.Pointer(unsafe.SliceData(c.mem)))
				c.release(int(uintptr(unsafe.Pointer(unsafe.SliceData(code)))-base), len(code))
			}
			out[i] = nil
		}
		return errors.Join(errs...)
	}
	for i, instructions := range list {
		size := alignUp(len(instructions), codeAlign)
		n := alignUp(shift+size, codeAlign)
		var c *chunk
		start := 0
		if k := len(segs); k > 0 && !segs[k-1].c.own && segs[k-1].c.used+n <= len(segs[k-1].c.mem) {
			c = segs[k-1].c
			start = c.used
		} else if o := a.open; o != nil && alignUp(o.used, page)+n <= len(o.mem) {
			c = o
			start = alignUp(c.used, page)
			if a.reuse && start > c.used {
				// The rest of the page before, for code placed later.
				c.release(c.used, start-c.used)
			}
			segs = append(segs, segment{c: c, from: start, first: i})
		} else {
			mem, err := mapCode(alignUp(n, chunkBytes))
			if err != nil {
				return nil, errors.Join(fmt.Errorf("allocate code: %w", err), undo())
			}
			c = &chunk{mem: mem, own: len(mem) > chunkBytes}
			if pad := alignUp(FirstOffset, codeAlign); len(a.chunks) == 0 && pad > 0 && pad+n <= len(mem) && !a.reuse {
				c.used = pad
			}
			a.chunks = append(a.chunks, c)
			if !c.own {
				a.open = c
			}
			start = c.used
			segs = append(segs, segment{c: c, from: start, first: i})
		}
		out[i] = c.mem[start+shift : start+shift+size : start+shift+size]
		c.used = start + n
		c.live += size
		segs[len(segs)-1].to = c.used
	}
	for k, s := range segs {
		last := len(list)
		if k+1 < len(segs) {
			last = segs[k+1].first
		}
		pages := s.c.mem[s.from/page*page : min(alignUp(s.to, page), len(s.c.mem))]
		if err := protectCode(pages, false); err != nil {
			return nil, errors.Join(fmt.Errorf("open code: %w", err), undo())
		}
		for i := s.first; i < last; i++ {
			copy(out[i], list[i])
		}
		if err := protectCode(pages, true); err != nil {
			return nil, errors.Join(fmt.Errorf("seal code: %w", err), undo())
		}
		for i := s.first; i < last; i++ {
			if err := flushCode(out[i]); err != nil {
				return nil, errors.Join(fmt.Errorf("flush instruction cache: %w", err), undo())
			}
		}
	}
	return out, nil
}

// chunkOf is the chunk holding code.
func (a *Arena) chunkOf(code []byte) *chunk {
	at := uintptr(unsafe.Pointer(unsafe.SliceData(code)))
	for _, c := range a.chunks {
		base := uintptr(unsafe.Pointer(unsafe.SliceData(c.mem)))
		if at >= base && at < base+uintptr(len(c.mem)) {
			return c
		}
	}
	panic("jit: code is not in its arena")
}

// placeReusing is place for a reusing arena (NewReusingArena): in the
// first space released code left that is large enough, else past the code
// of the first chunk with room for it there that is not a function's own,
// else in a new chunk. a.mu is held.
func (a *Arena) placeReusing(instructions []byte) ([]byte, error) {
	n := alignUp(len(instructions), codeAlign)
	var c *chunk
	start := -1
	for _, ch := range a.chunks {
		for i, s := range ch.free {
			if s.n < n {
				continue
			}
			c, start = ch, s.off
			if s.n == n {
				ch.free = append(ch.free[:i], ch.free[i+1:]...)
			} else {
				ch.free[i] = span{s.off + n, s.n - n}
			}
			break
		}
		if c != nil {
			break
		}
	}
	if c == nil {
		for _, ch := range a.chunks {
			if !ch.own && ch.used+n <= len(ch.mem) {
				c = ch
				break
			}
		}
		if c == nil {
			mem, err := mapCode(alignUp(n, chunkBytes))
			if err != nil {
				return nil, fmt.Errorf("allocate code: %w", err)
			}
			c = &chunk{mem: mem, own: len(mem) > chunkBytes}
			a.chunks = append(a.chunks, c)
			if !c.own {
				a.open = c
			}
		}
		start = c.used
		c.used += n
	}
	page := os.Getpagesize()
	pages := c.mem[start/page*page : min(alignUp(start+n, page), len(c.mem))]
	code := c.mem[start : start+n : start+n]
	if err := a.write(c, pages, code, instructions); err != nil {
		if c.live == 0 {
			err = errors.Join(err, a.unmap(c))
		} else {
			c.release(start, n)
		}
		return nil, err
	}
	c.live += n
	return code, nil
}

// release makes the n bytes from off space to reuse, joined to the space
// next to them.
func (c *chunk) release(off, n int) {
	i := 0
	for i < len(c.free) && c.free[i].off < off {
		i++
	}
	if i > 0 && c.free[i-1].off+c.free[i-1].n == off {
		i--
		c.free[i].n += n
	} else {
		c.free = append(c.free, span{})
		copy(c.free[i+1:], c.free[i:])
		c.free[i] = span{off, n}
	}
	if i+1 < len(c.free) && c.free[i].off+c.free[i].n == c.free[i+1].off {
		c.free[i].n += c.free[i+1].n
		c.free = append(c.free[:i+1], c.free[i+2:]...)
	}
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
	if a.open == c {
		// Code goes on in the last chunk that is not a function's own.
		a.open = nil
		for _, o := range slices.Backward(a.chunks) {
			if !o.own {
				a.open = o
				break
			}
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
			if a.reuse {
				c.release(int(at-base), len(code))
			}
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
