package wasi_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
	"github.com/go-quickjs/go-quickjs/wasm/wasi"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

var le = binary.LittleEndian

// harness is a module that calls each WASI function through an export of
// the same name and signature, with a page of memory for its arguments.
type harness struct {
	t   *testing.T
	ctx context.Context
	mod api.Module
	mem api.Memory
}

// The module's memory is laid out for the tests: what they pass at scratch
// and below, what functions write at out.
const (
	scratch = 0x100
	out     = 0x8000
	memSize = 0x10000
)

func valType(t api.ValueType) string {
	if t == api.ValueTypeI64 {
		return "i64"
	}
	return "i32"
}

// wrapper returns the text of a module that imports every function of the
// WASI module defs describe and exports a function calling each.
func wrapper(defs map[string]api.FunctionDefinition) string {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("(module\n")
	sig := func(d api.FunctionDefinition) string {
		var s strings.Builder
		for _, p := range d.ParamTypes() {
			s.WriteString(" (param " + valType(p) + ")")
		}
		for _, r := range d.ResultTypes() {
			s.WriteString(" (result " + valType(r) + ")")
		}
		return s.String()
	}
	for _, n := range names {
		fmt.Fprintf(&b, "  (import %q %q (func $%s%s))\n", wasi.ModuleName, n, n, sig(defs[n]))
	}
	b.WriteString("  (memory (export \"memory\") 1)\n")
	for _, n := range names {
		d := defs[n]
		fmt.Fprintf(&b, "  (func (export %q)%s", n, sig(d))
		for i := range d.ParamTypes() {
			fmt.Fprintf(&b, " local.get %d", i)
		}
		fmt.Fprintf(&b, " call $%s)\n", n)
	}
	b.WriteString(")\n")
	return b.String()
}

func newHarness(t *testing.T, cfg wasm.ModuleConfig) *harness {
	t.Helper()
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	closer, err := wasi.Instantiate(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	defs := closer.(api.Module).ExportedFunctionDefinitions()
	bin, err := wat.Compile([]byte(wrapper(defs)))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := r.InstantiateWithConfig(ctx, bin, cfg.WithName(""))
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, ctx: ctx, mod: mod, mem: mod.Memory()}
}

// call calls WASI function name and returns its errno's name.
func (h *harness) call(name string, args ...uint64) string {
	h.t.Helper()
	res, err := h.mod.ExportedFunction(name).Call(h.ctx, args...)
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return wasi.ErrnoName(uint32(res[0]))
}

// expect calls name and fails the test unless it returns errno want.
func (h *harness) expect(want, name string, args ...uint64) {
	h.t.Helper()
	if got := h.call(name, args...); got != want {
		h.t.Errorf("%s%v = %s, want %s", name, args, got, want)
	}
}

// str writes s at off and returns its offset and length.
func (h *harness) str(off uint32, s string) (uint64, uint64) {
	h.mem.WriteString(off, s)
	return uint64(off), uint64(len(s))
}

func (h *harness) u32(off uint32) uint32 {
	v, _ := h.mem.ReadUint32Le(off)
	return v
}

func (h *harness) u64(off uint32) uint64 {
	v, _ := h.mem.ReadUint64Le(off)
	return v
}

func (h *harness) bytes(off, n uint32) []byte {
	b, _ := h.mem.Read(off, n)
	return append([]byte(nil), b...)
}

// iovec writes an iovec of buf, len n, at off.
func (h *harness) iovec(off, buf, n uint32) {
	h.mem.WriteUint32Le(off, buf)
	h.mem.WriteUint32Le(off+4, n)
}

// open opens path under directory fd and returns the new descriptor.
func (h *harness) open(dirfd uint64, path string, oflags uint64, rights uint64, fdflags uint64) (uint64, string) {
	h.t.Helper()
	p, n := h.str(scratch, path)
	errno := h.call("path_open", dirfd, 1, p, n, oflags, rights, 0, fdflags, out)
	return uint64(h.u32(out)), errno
}

// write writes s to fd and returns the errno and the count written.
func (h *harness) write(fd uint64, s string) (string, uint32) {
	h.t.Helper()
	h.mem.WriteString(scratch+16, s)
	h.iovec(scratch, scratch+16, uint32(len(s)))
	errno := h.call("fd_write", fd, scratch, 1, out)
	return errno, h.u32(out)
}

// read reads up to n bytes from fd.
func (h *harness) read(fd uint64, n uint32) (string, string) {
	h.t.Helper()
	h.iovec(scratch, scratch+16, n)
	errno := h.call("fd_read", fd, scratch, 1, out)
	return errno, string(h.bytes(scratch+16, h.u32(out)))
}

const (
	rightRead  = 1 << 1
	rightWrite = 1 << 6
	oCreat     = 1
	oDirectory = 2
	oExcl      = 4
	oTrunc     = 8
	preopen    = 3 // the first mount's descriptor
)

func TestArgsEnviron(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig().WithArgs("prog", "x").WithEnv("K", "v"))
	h.expect("ESUCCESS", "args_sizes_get", out, out+4)
	if h.u32(out) != 2 || h.u32(out+4) != 7 {
		t.Errorf("args sizes %d %d", h.u32(out), h.u32(out+4))
	}
	h.expect("ESUCCESS", "args_get", out, out+16)
	if p0, p1 := h.u32(out), h.u32(out+4); p0 != out+16 || p1 != out+21 {
		t.Errorf("argv %#x %#x", p0, p1)
	}
	if got := h.bytes(out+16, 7); string(got) != "prog\x00x\x00" {
		t.Errorf("argv_buf %q", got)
	}
	h.expect("ESUCCESS", "environ_sizes_get", out, out+4)
	if h.u32(out) != 1 || h.u32(out+4) != 4 {
		t.Errorf("environ sizes %d %d", h.u32(out), h.u32(out+4))
	}
	h.expect("ESUCCESS", "environ_get", out, out+16)
	if got := h.bytes(out+16, 4); string(got) != "K=v\x00" {
		t.Errorf("environ_buf %q", got)
	}
	h.expect("EFAULT", "args_get", memSize-2, out)
	h.expect("EFAULT", "args_get", out, memSize-3)
	h.expect("EFAULT", "args_sizes_get", memSize, out)
	h.expect("EFAULT", "environ_sizes_get", out, memSize-1)
}

func TestClock(t *testing.T) {
	cfg := wasm.NewModuleConfig().
		WithWalltime(func() (int64, int32) { return 5, 6 }, 1000).
		WithNanotime(func() int64 { return 42 }, 7)
	h := newHarness(t, cfg)
	h.expect("ESUCCESS", "clock_res_get", 0, out)
	h.expect("ESUCCESS", "clock_res_get", 1, out+8)
	if h.u64(out) != 1000 || h.u64(out+8) != 7 {
		t.Errorf("resolutions %d %d", h.u64(out), h.u64(out+8))
	}
	h.expect("ESUCCESS", "clock_time_get", 0, 1, out)
	h.expect("ESUCCESS", "clock_time_get", 1, 1, out+8)
	if h.u64(out) != 5_000_000_006 || h.u64(out+8) != 42 {
		t.Errorf("times %d %d", h.u64(out), h.u64(out+8))
	}
	for _, id := range []uint64{2, 3, 4} {
		h.expect("EINVAL", "clock_res_get", id, out)
		h.expect("EINVAL", "clock_time_get", id, 0, out)
	}
	h.expect("EFAULT", "clock_time_get", 0, 0, memSize-4)
	h.expect("EFAULT", "clock_res_get", 1, memSize)
}

func TestStdio(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg := wasm.NewModuleConfig().WithStdin(strings.NewReader("input")).WithStdout(&stdout).WithStderr(&stderr)
	h := newHarness(t, cfg)
	if errno, n := h.write(1, "hello"); errno != "ESUCCESS" || n != 5 || stdout.String() != "hello" {
		t.Errorf("stdout: %s %d %q", errno, n, stdout.String())
	}
	if errno, _ := h.write(2, "oops"); errno != "ESUCCESS" || stderr.String() != "oops" {
		t.Errorf("stderr: %s %q", errno, stderr.String())
	}
	// Two iovecs, one of them empty.
	h.mem.WriteString(scratch+32, "ab")
	h.iovec(scratch, scratch+32, 1)
	h.iovec(scratch+8, scratch+33, 0)
	h.iovec(scratch+16, scratch+33, 1)
	h.expect("ESUCCESS", "fd_write", 1, scratch, 3, out)
	if h.u32(out) != 2 || stdout.String() != "helloab" {
		t.Errorf("writev: %d %q", h.u32(out), stdout.String())
	}
	if errno, s := h.read(0, 3); errno != "ESUCCESS" || s != "inp" {
		t.Errorf("stdin: %s %q", errno, s)
	}
	if errno, s := h.read(0, 10); errno != "ESUCCESS" || s != "ut" {
		t.Errorf("stdin rest: %s %q", errno, s)
	}
	if errno, s := h.read(0, 10); errno != "ESUCCESS" || s != "" {
		t.Errorf("stdin end: %s %q", errno, s)
	}
	h.expect("ESPIPE", "fd_seek", 1, 0, 0, out)
	h.expect("ESPIPE", "fd_tell", 0, out)
	h.expect("ESPIPE", "fd_pwrite", 1, scratch, 1, 0, out)
	h.expect("EBADF", "fd_read", 1, scratch, 1, out)
	h.expect("EBADF", "fd_write", 0, scratch, 1, out)

	h.expect("ESUCCESS", "fd_fdstat_get", 1, out)
	if ft := h.bytes(out, 1)[0]; ft != 2 {
		t.Errorf("stdout file type %d, want character device", ft)
	}
	if rights := h.u64(out + 8); rights&(1<<2) != 0 || rights&rightWrite == 0 {
		t.Errorf("stdout rights %#x", rights)
	}
	h.expect("EBADF", "fd_prestat_get", 1, out)
}

func TestBadDescriptors(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig())
	for _, fd := range []uint64{3, 99, 0xffffffff} {
		h.expect("EBADF", "fd_write", fd, scratch, 0, out)
		h.expect("EBADF", "fd_read", fd, scratch, 0, out)
		h.expect("EBADF", "fd_close", fd)
		h.expect("EBADF", "fd_seek", fd, 0, 0, out)
		h.expect("EBADF", "fd_tell", fd, out)
		h.expect("EBADF", "fd_fdstat_get", fd, out)
		h.expect("EBADF", "fd_fdstat_set_flags", fd, 0)
		h.expect("EBADF", "fd_fdstat_set_rights", fd, 0, 0)
		h.expect("EBADF", "fd_filestat_get", fd, out)
		h.expect("EBADF", "fd_filestat_set_size", fd, 0)
		h.expect("EBADF", "fd_filestat_set_times", fd, 0, 0, 0)
		h.expect("EBADF", "fd_pread", fd, scratch, 0, 0, out)
		h.expect("EBADF", "fd_pwrite", fd, scratch, 0, 0, out)
		h.expect("EBADF", "fd_prestat_get", fd, out)
		h.expect("EBADF", "fd_prestat_dir_name", fd, out, 10)
		h.expect("EBADF", "fd_readdir", fd, out, 100, 0, out)
		h.expect("EBADF", "fd_renumber", fd, 1)
		h.expect("EBADF", "fd_renumber", 1, fd)
		h.expect("EBADF", "fd_advise", fd, 0, 0, 0)
		h.expect("EBADF", "fd_allocate", fd, 0, 0)
		h.expect("EBADF", "fd_sync", fd)
		h.expect("EBADF", "fd_datasync", fd)
		h.expect("EBADF", "path_open", fd, 0, scratch, 1, 0, 0, 0, 0, out)
		h.expect("EBADF", "path_create_directory", fd, scratch, 1)
		h.expect("EBADF", "path_unlink_file", fd, scratch, 1)
		h.expect("EBADF", "sock_accept", fd, 0, out)
		h.expect("EBADF", "sock_recv", fd, scratch, 0, 0, out, out+4)
		h.expect("EBADF", "sock_send", fd, scratch, 0, 0, out)
		h.expect("EBADF", "sock_shutdown", fd, 3)
	}
	// Not a directory.
	h.expect("ENOTDIR", "path_open", 1, 0, scratch, 1, 0, 0, 0, 0, out)
	h.expect("ENOTDIR", "fd_readdir", 1, out, 100, 0, out)
	// Not a socket.
	h.expect("ENOTSOCK", "sock_accept", 1, 0, out)
	h.expect("ENOTSOCK", "sock_send", 1, scratch, 0, 0, out)
	h.expect("ENOTSOCK", "sock_recv", 0, scratch, 0, 0, out, out+4)
	h.expect("ENOTSOCK", "sock_shutdown", 1, 3)
	h.expect("ENOSYS", "proc_raise", 9)
	h.expect("ESUCCESS", "sched_yield")
}

func TestOutOfBounds(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(t.TempDir(), "/")))
	// An iovec array, or a buffer one names, past the end.
	h.expect("EFAULT", "fd_write", 1, memSize-4, 1, out)
	h.expect("EFAULT", "fd_write", 1, scratch, 0x20000000, out)
	h.iovec(scratch, memSize-2, 4)
	h.expect("EFAULT", "fd_write", 1, scratch, 1, out)
	h.expect("EFAULT", "fd_read", 0, scratch, 1, out)
	h.iovec(scratch, scratch+16, 1)
	h.expect("EFAULT", "fd_write", 1, scratch, 1, memSize-3)
	h.expect("EFAULT", "fd_fdstat_get", 1, memSize-20)
	h.expect("EFAULT", "fd_filestat_get", preopen, memSize-60)
	h.expect("EFAULT", "fd_prestat_get", preopen, memSize-4)
	h.expect("EFAULT", "fd_prestat_dir_name", preopen, memSize, 1)
	h.expect("EFAULT", "fd_readdir", preopen, memSize-10, 100, 0, out)
	h.expect("EFAULT", "path_open", preopen, 0, memSize-1, 5, 0, 0, 0, 0, out)
	p, n := h.str(scratch+512, ".")
	h.expect("EFAULT", "path_filestat_get", preopen, 0, p, n, memSize-8)
	h.expect("EFAULT", "random_get", memSize-8, 9)
	h.expect("EFAULT", "poll_oneoff", memSize-40, out, 1, out)
	h.expect("EFAULT", "poll_oneoff", scratch, memSize-16, 1, out)
	h.expect("EFAULT", "clock_time_get", 0, 0, 0xffffffff)
	// A path_open whose result does not fit closes what it opened.
	h.mem.WriteString(scratch, "f")
	h.expect("EFAULT", "path_open", preopen, 0, scratch, 1, oCreat, rightRead|rightWrite, 0, 0, memSize-2)
	h.expect("EBADF", "fd_close", 4)
}

// mountDir mounts a temporary directory at "/" and returns it and its
// harness.
func mountDir(t *testing.T) (string, *harness) {
	dir := t.TempDir()
	return dir, newHarness(t, wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/")))
}

func TestPreopens(t *testing.T) {
	cfg := wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().
		WithDirMount(t.TempDir(), "/").
		WithFSMount(fstest.MapFS{}, "data/").
		WithReadOnlyDirMount(t.TempDir(), "/").
		WithDirMount(t.TempDir(), "tmp"))
	h := newHarness(t, cfg)
	for i, name := range []string{"/", "data/", "tmp"} {
		fd := uint64(preopen + i)
		h.expect("ESUCCESS", "fd_prestat_get", fd, out)
		if h.u32(out) != 0 || h.u32(out+4) != uint32(len(name)) {
			t.Errorf("prestat of %d: %d %d", fd, h.u32(out), h.u32(out+4))
		}
		h.expect("ESUCCESS", "fd_prestat_dir_name", fd, out, uint64(len(name)))
		if got := string(h.bytes(out, uint32(len(name)))); got != name {
			t.Errorf("name of %d: %q, want %q", fd, got, name)
		}
		h.expect("ENAMETOOLONG", "fd_prestat_dir_name", fd, out, uint64(len(name)-1))
		h.expect("ESUCCESS", "fd_fdstat_get", fd, out)
		if ft := h.bytes(out, 1)[0]; ft != 3 {
			t.Errorf("file type of %d: %d", fd, ft)
		}
	}
	h.expect("EBADF", "fd_prestat_get", 6, out)
	// The read-only mount replaced the first at "/".
	h.mem.WriteString(scratch, "x")
	h.expect("EROFS", "path_create_directory", preopen, scratch, 1)
	h.expect("EROFS", "path_create_directory", preopen+1, scratch, 1)
	h.expect("ESUCCESS", "path_create_directory", preopen+2, scratch, 1)
}

func TestFileIO(t *testing.T) {
	dir, h := mountDir(t)
	fd, errno := h.open(preopen, "f.txt", oCreat|oExcl, rightRead|rightWrite, 0)
	if errno != "ESUCCESS" || fd != 4 {
		t.Fatalf("open: %s %d", errno, fd)
	}
	if _, errno := h.open(preopen, "f.txt", oCreat|oExcl, rightRead|rightWrite, 0); errno != "EEXIST" {
		t.Errorf("exclusive create of an existing file: %s", errno)
	}
	if errno, n := h.write(fd, "0123456789"); errno != "ESUCCESS" || n != 10 {
		t.Fatalf("write: %s %d", errno, n)
	}
	h.expect("ESUCCESS", "fd_tell", fd, out)
	if h.u64(out) != 10 {
		t.Errorf("tell %d", h.u64(out))
	}
	h.expect("ESUCCESS", "fd_seek", fd, 2, 0, out)
	if errno, s := h.read(fd, 3); errno != "ESUCCESS" || s != "234" {
		t.Errorf("read after seek: %s %q", errno, s)
	}
	h.expect("ESUCCESS", "fd_seek", fd, uint64(^uint64(1)), 1, out) // -2 from the current
	if h.u64(out) != 3 {
		t.Errorf("seek back to %d", h.u64(out))
	}
	h.expect("ESUCCESS", "fd_seek", fd, uint64(^uint64(0)), 2, out) // -1 from the end
	if h.u64(out) != 9 {
		t.Errorf("seek from the end to %d", h.u64(out))
	}
	h.expect("EINVAL", "fd_seek", fd, 0, 3, out)
	h.expect("EINVAL", "fd_seek", fd, uint64(^uint64(99)), 0, out)

	// pread and pwrite leave the offset where it is.
	h.iovec(scratch, scratch+16, 4)
	h.expect("ESUCCESS", "fd_pread", fd, scratch, 1, 6, out)
	if h.u32(out) != 4 || string(h.bytes(scratch+16, 4)) != "6789" {
		t.Errorf("pread %d %q", h.u32(out), h.bytes(scratch+16, 4))
	}
	h.mem.WriteString(scratch+16, "AB")
	h.iovec(scratch, scratch+16, 2)
	h.expect("ESUCCESS", "fd_pwrite", fd, scratch, 1, 0, out)
	h.expect("ESUCCESS", "fd_tell", fd, out)
	if h.u64(out) != 9 {
		t.Errorf("tell after pread and pwrite %d", h.u64(out))
	}
	h.iovec(scratch, scratch+16, 4)
	h.expect("ESUCCESS", "fd_pread", fd, scratch, 1, 100, out)
	if h.u32(out) != 0 {
		t.Errorf("pread past the end read %d", h.u32(out))
	}

	h.expect("ESUCCESS", "fd_filestat_get", fd, out)
	if size, ft := h.u64(out+32), h.bytes(out+16, 1)[0]; size != 10 || ft != 4 {
		t.Errorf("filestat size %d type %d", size, ft)
	}
	h.expect("ESUCCESS", "fd_filestat_set_size", fd, 4)
	h.expect("ESUCCESS", "fd_allocate", fd, 2, 8)
	h.expect("ESUCCESS", "fd_filestat_get", fd, out)
	if size := h.u64(out + 32); size != 10 {
		t.Errorf("size after truncate and allocate %d", size)
	}
	h.expect("ESUCCESS", "fd_advise", fd, 0, 10, 4)
	h.expect("EINVAL", "fd_advise", fd, 0, 10, 6)
	h.expect("ESUCCESS", "fd_sync", fd)
	h.expect("ESUCCESS", "fd_datasync", fd)
	h.expect("ESUCCESS", "fd_close", fd)
	h.expect("EBADF", "fd_close", fd)
	if b, err := os.ReadFile(filepath.Join(dir, "f.txt")); err != nil || string(b) != "AB23\x00\x00\x00\x00\x00\x00" {
		t.Errorf("host sees %q, %v", b, err)
	}

	// Append mode writes at the end; pwrite writes at its offset, and
	// leaves the file's where it was.
	fd, _ = h.open(preopen, "f.txt", oTrunc, rightRead|rightWrite, 1)
	h.write(fd, "abc")
	h.expect("ESUCCESS", "fd_seek", fd, 0, 0, out)
	h.write(fd, "def")
	h.mem.WriteString(scratch+16, "g")
	h.iovec(scratch, scratch+16, 1)
	h.expect("ESUCCESS", "fd_pwrite", fd, scratch, 1, 0, out)
	h.expect("ESUCCESS", "fd_tell", fd, out)
	if h.u64(out) != 6 {
		t.Errorf("tell after pwrite in append mode %d", h.u64(out))
	}
	h.expect("ESUCCESS", "fd_fdstat_get", fd, out)
	if flags := le.Uint16(h.bytes(out+2, 2)); flags != 1 {
		t.Errorf("fdflags %d, want append", flags)
	}
	h.expect("ESUCCESS", "fd_fdstat_set_flags", fd, 0)
	h.expect("ESUCCESS", "fd_seek", fd, 0, 0, out)
	h.write(fd, "X")
	h.expect("EINVAL", "fd_fdstat_set_flags", fd, 0x20)
	h.expect("ESUCCESS", "fd_close", fd)
	if b, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(b) != "Xbcdef" {
		t.Errorf("after appends %q", b)
	}

	// A file opened for reading only.
	fd, _ = h.open(preopen, "f.txt", 0, rightRead, 0)
	if errno, _ := h.write(fd, "no"); errno != "EBADF" {
		t.Errorf("write to a read-only descriptor: %s", errno)
	}
	h.expect("EBADF", "fd_filestat_set_size", fd, 0)
	h.expect("ESUCCESS", "fd_fdstat_get", fd, out)
	if rights := h.u64(out + 8); rights&rightWrite != 0 || rights&rightRead == 0 {
		t.Errorf("rights of a read-only descriptor %#x", rights)
	}
	// Rights may be reduced, never raised.
	h.expect("ESUCCESS", "fd_fdstat_set_rights", fd, rightRead, 0)
	h.expect("ENOTCAPABLE", "fd_fdstat_set_rights", fd, rightRead|rightWrite, 0)
	h.expect("ESUCCESS", "fd_fdstat_get", fd, out)
	if rights := h.u64(out + 8); rights != rightRead {
		t.Errorf("reduced rights %#x", rights)
	}
	// What needs a dropped right fails.
	h.expect("ENOTCAPABLE", "fd_seek", fd, 0, 0, out)
	h.expect("ENOTCAPABLE", "fd_filestat_get", fd, out)
	h.expect("ESUCCESS", "fd_fdstat_set_rights", fd, 0, 0)
	h.expect("ENOTCAPABLE", "fd_read", fd, scratch, 0, out)
	// A directory's rights restrict the paths under it.
	dfd, _ := h.open(preopen, ".", oDirectory, 0, 0)
	h.expect("ESUCCESS", "fd_fdstat_get", dfd, out)
	h.expect("ESUCCESS", "fd_fdstat_set_rights", dfd, h.u64(out+8)&^(1<<19), h.u64(out+16))
	if _, errno := h.open(dfd, "f.txt", oTrunc, rightRead|rightWrite, 0); errno != "ENOTCAPABLE" {
		t.Errorf("truncate without path_filestat_set_size: %s", errno)
	}
	if fd, errno := h.open(dfd, "f.txt", 0, rightRead, 0); errno != "ESUCCESS" {
		t.Errorf("open without truncating: %s", errno)
	} else {
		h.call("fd_close", fd)
	}
	h.call("fd_close", dfd)

	// Renumbering replaces the target.
	fd2, _ := h.open(preopen, "f.txt", 0, rightRead, 0)
	h.expect("ESUCCESS", "fd_renumber", fd2, fd)
	h.expect("EBADF", "fd_close", fd2)
	if errno, s := h.read(fd, 3); errno != "ESUCCESS" || s != "Xbc" {
		t.Errorf("read renumbered: %s %q", errno, s)
	}
	h.expect("ESUCCESS", "fd_renumber", fd, fd)
	h.expect("EBADF", "fd_renumber", fd, fd2)
	h.expect("ESUCCESS", "fd_close", fd)
}

func TestDirectories(t *testing.T) {
	dir, h := mountDir(t)
	mkdir := func(p string) string {
		ptr, n := h.str(scratch, p)
		return h.call("path_create_directory", preopen, ptr, n)
	}
	if errno := mkdir("d"); errno != "ESUCCESS" {
		t.Fatal(errno)
	}
	if errno := mkdir("d"); errno != "EEXIST" {
		t.Errorf("mkdir twice: %s", errno)
	}
	if errno := mkdir("d/e/f"); errno != "ENOENT" {
		t.Errorf("mkdir with a missing parent: %s", errno)
	}
	mkdir("d/e/")
	for _, name := range []string{"d/one", "d/two", "d/three"} {
		fd, errno := h.open(preopen, name, oCreat, rightWrite, 0)
		if errno != "ESUCCESS" {
			t.Fatal(name, errno)
		}
		h.call("fd_close", fd)
	}
	dfd, errno := h.open(preopen, "d", oDirectory, 0, 0)
	if errno != "ESUCCESS" {
		t.Fatal(errno)
	}
	if _, errno := h.open(preopen, "d/one", oDirectory, 0, 0); errno != "ENOTDIR" {
		t.Errorf("open a file as a directory: %s", errno)
	}
	if _, errno := h.open(preopen, "d", oDirectory|oCreat, 0, 0); errno != "EINVAL" {
		t.Errorf("create a directory by path_open: %s", errno)
	}
	if _, errno := h.open(preopen, "d", 0, rightWrite, 0); errno != "EISDIR" {
		t.Errorf("open a directory for writing: %s", errno)
	}
	h.expect("EISDIR", "fd_read", dfd, scratch, 0, out)
	h.expect("EBADF", "fd_write", dfd, scratch, 0, out)
	h.expect("EISDIR", "fd_seek", dfd, 0, 0, out)

	// The listing, in one call and in pieces.
	readdir := func(bufLen uint32, cookie uint64) []byte {
		h.expect("ESUCCESS", "fd_readdir", dfd, out, uint64(bufLen), cookie, scratch)
		return h.bytes(out, h.u32(scratch))
	}
	type ent struct {
		next uint64
		name string
		typ  byte
	}
	parse := func(b []byte) (ents []ent) {
		for len(b) >= 24 {
			n := le.Uint32(b[16:])
			if uint32(len(b)) < 24+n {
				break
			}
			ents = append(ents, ent{le.Uint64(b), string(b[24 : 24+n]), b[20]})
			b = b[24+n:]
		}
		return ents
	}
	all := parse(readdir(4096, 0))
	var names []string
	for i, e := range all {
		if e.next != uint64(i+1) {
			t.Errorf("entry %d has d_next %d", i, e.next)
		}
		names = append(names, fmt.Sprintf("%s:%d", e.name, e.typ))
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "..:3 .:3 e:3 one:4 three:4 two:4" {
		t.Errorf("listing %s", got)
	}
	var pieces []string
	for cookie := uint64(0); ; {
		b := readdir(40, cookie)
		es := parse(b)
		if len(es) == 0 {
			if len(b) != 0 && len(b) < 40 {
				t.Errorf("a short buffer ended in a partial entry")
			}
			break
		}
		for _, e := range es {
			pieces = append(pieces, e.name)
		}
		cookie = es[len(es)-1].next
	}
	if len(pieces) != len(all) {
		t.Errorf("listing in pieces %v", pieces)
	}
	if b := readdir(10, 0); len(b) != 10 {
		t.Errorf("a buffer too small for an entry got %d bytes", len(b))
	}
	if b := readdir(4096, 100); len(b) != 0 {
		t.Errorf("a cookie past the end listed %d bytes", len(b))
	}
	h.expect("EFAULT", "fd_readdir", dfd, out, 100, 0, memSize)

	// Paths relative to an opened directory.
	sub, _ := h.open(dfd, "e", oDirectory, 0, 0)
	// A path cannot leave the directory it is relative to, even to come
	// back into it.
	for _, p := range []string{"../one", "../e/x", "../../../x", "x/../../e"} {
		if _, errno := h.open(sub, p, oCreat, rightRead, 0); errno != "EPERM" {
			t.Errorf("open %s under d/e: %s", p, errno)
		}
	}
	if fd, errno := h.open(sub, "x/../y", oCreat, rightRead, 0); errno != "ESUCCESS" {
		t.Errorf("open x/../y under d/e: %s", errno)
	} else {
		h.call("fd_close", fd)
		os.Remove(filepath.Join(dir, "d", "e", "y"))
	}

	h.call("fd_close", sub)
	h.call("fd_close", dfd)

	rmdir := func(p string) string {
		ptr, n := h.str(scratch, p)
		return h.call("path_remove_directory", preopen, ptr, n)
	}
	unlink := func(p string) string {
		ptr, n := h.str(scratch, p)
		return h.call("path_unlink_file", preopen, ptr, n)
	}
	for _, c := range []struct{ errno, got string }{
		{"ENOTEMPTY", rmdir("d")},
		{"ENOTDIR", rmdir("d/one")},
		{"EISDIR", unlink("d/e")},
		{"ENOENT", unlink("d/nope")},
		{"ENOTDIR", unlink("d/one/")},
		{"ESUCCESS", unlink("d/one")},
		{"ESUCCESS", rmdir("d/e/")},
		{"ENOENT", rmdir("d/e")},
		{"EPERM", rmdir("..")},
	} {
		if c.got != c.errno {
			t.Errorf("got %s, want %s", c.got, c.errno)
		}
	}
	// Rename.
	rename := func(from, to string) string {
		fp, fn := h.str(scratch, from)
		tp, tn := h.str(scratch+256, to)
		return h.call("path_rename", preopen, fp, fn, preopen, tp, tn)
	}
	mkdir("empty")
	for _, c := range []struct{ errno, got string }{
		{"ESUCCESS", rename("d/two", "d/2")},
		{"ESUCCESS", rename("d/2", "d/three")}, // replaces a file
		{"EISDIR", rename("d/three", "empty")},
		{"ENOTDIR", rename("empty", "d/three")},
		{"ENOTDIR", rename("d/three/", "x")},
		{"ESUCCESS", rename("empty", "d/e")},
		{"ENOENT", rename("nope", "x")},
		{"EPERM", rename("d", "../d")},
	} {
		if c.got != c.errno {
			t.Errorf("rename: got %s, want %s", c.got, c.errno)
		}
	}
	got := []string{}
	ents, _ := os.ReadDir(filepath.Join(dir, "d"))
	for _, e := range ents {
		got = append(got, e.Name())
	}
	if strings.Join(got, " ") != "e three" {
		t.Errorf("d holds %v", got)
	}
}

func TestPathStatAndTimes(t *testing.T) {
	dir, h := mountDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("12345"), 0o644)
	p, n := h.str(scratch, "f")
	h.expect("ESUCCESS", "path_filestat_get", preopen, 1, p, n, out)
	if size, ft, nlink := h.u64(out+32), h.bytes(out+16, 1)[0], h.u64(out+24); size != 5 || ft != 4 || nlink < 1 {
		t.Errorf("filestat size %d type %d nlink %d", size, ft, nlink)
	}
	p, n = h.str(scratch, "f/")
	h.expect("ENOTDIR", "path_filestat_get", preopen, 1, p, n, out)
	p, n = h.str(scratch, ".")
	h.expect("ESUCCESS", "path_filestat_get", preopen, 0, p, n, out)
	if ft := h.bytes(out+16, 1)[0]; ft != 3 {
		t.Errorf("the root's type %d", ft)
	}

	const atim, mtim = 1_000_000_000_000_000_000, 1_200_000_000_000_000_000
	p, n = h.str(scratch, "f")
	h.expect("ESUCCESS", "path_filestat_set_times", preopen, 1, p, n, atim, mtim, 1|4)
	fi, err := os.Stat(filepath.Join(dir, "f"))
	if err != nil || fi.ModTime().UnixNano() != mtim {
		t.Errorf("mtime %v, %v", fi.ModTime(), err)
	}
	h.expect("EINVAL", "path_filestat_set_times", preopen, 1, p, n, 0, 0, 1|2)
	h.expect("EINVAL", "path_filestat_set_times", preopen, 1, p, n, 0, 0, 4|8)
	// Only the access time: the modification time stays.
	h.expect("ESUCCESS", "path_filestat_set_times", preopen, 1, p, n, atim, 0, 1)
	if fi, _ := os.Stat(filepath.Join(dir, "f")); fi.ModTime().UnixNano() != mtim {
		t.Errorf("mtime changed to %v", fi.ModTime())
	}
	// Now, by the module's wall clock.
	cfgNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	h2 := newHarness(t, wasm.NewModuleConfig().
		WithWalltime(func() (int64, int32) { return cfgNow.Unix(), 0 }, 1).
		WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/")))
	p, n = h2.str(scratch, "f")
	h2.expect("ESUCCESS", "path_filestat_set_times", preopen, 1, p, n, 0, 0, 8)
	if fi, _ := os.Stat(filepath.Join(dir, "f")); !fi.ModTime().Equal(cfgNow) {
		t.Errorf("mtime %v, want %v", fi.ModTime(), cfgNow)
	}
	fd, _ := h2.open(preopen, "f", 0, rightRead|rightWrite, 0)
	h2.expect("ESUCCESS", "fd_filestat_set_times", fd, 0, mtim, 4)
	if fi, _ := os.Stat(filepath.Join(dir, "f")); fi.ModTime().UnixNano() != mtim {
		t.Errorf("mtime by descriptor %v", fi.ModTime())
	}
}

func TestPathEscapes(t *testing.T) {
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644)
	dir, h := mountDir(t)
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("f"), 0o644)
	for _, p := range []string{
		"..", "../", "../secret", "sub/../../secret", "sub/../..", "/etc/passwd", "/",
		"a/b/../../../x",
	} {
		if _, errno := h.open(preopen, p, 0, rightRead, 0); errno != "EPERM" {
			t.Errorf("open %q: %s, want EPERM", p, errno)
		}
		ptr, n := h.str(scratch, p)
		h.expect("EPERM", "path_filestat_get", preopen, 1, ptr, n, out)
		h.expect("EPERM", "path_create_directory", preopen, ptr, n)
		h.expect("EPERM", "path_unlink_file", preopen, ptr, n)
	}
	for _, p := range []string{"sub/../sub/f", "./sub/./f", "sub//f"} {
		if fd, errno := h.open(preopen, p, 0, rightRead, 0); errno != "ESUCCESS" {
			t.Errorf("open %q: %s", p, errno)
		} else {
			h.call("fd_close", fd)
		}
	}
	h.expect("ENOENT", "path_open", preopen, 0, scratch, 0, 0, rightRead, 0, 0, out)
	h.mem.Write(scratch, []byte{0xff, 0xfe})
	h.expect("EILSEQ", "path_open", preopen, 0, scratch, 2, 0, rightRead, 0, 0, out)

	// Symbolic links that leave the mount.
	rel, _ := filepath.Rel(dir, outside)
	if err := os.Symlink(outside, filepath.Join(dir, "abs")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	os.Symlink(rel, filepath.Join(dir, "rel"))
	os.Symlink(filepath.Join("..", rel, "secret"), filepath.Join(dir, "sub", "deep"))
	os.Symlink("sub/f", filepath.Join(dir, "good"))
	for _, p := range []string{"abs/secret", "rel/secret", "sub/deep"} {
		if _, errno := h.open(preopen, p, 0, rightRead, 0); errno != "EPERM" {
			t.Errorf("open %q: %s, want EPERM", p, errno)
		}
		ptr, n := h.str(scratch, p)
		h.expect("EPERM", "path_filestat_get", preopen, 1, ptr, n, out)
		h.expect("EPERM", "path_filestat_set_times", preopen, 1, ptr, n, 0, 0, 8)
	}
	for _, p := range []string{"rel/x", "abs/x"} {
		ptr, n := h.str(scratch, p)
		h.expect("EPERM", "path_create_directory", preopen, ptr, n)
		tp, tn := h.str(scratch+256, "sub/f")
		if got := h.call("path_rename", preopen, tp, tn, preopen, ptr, n); got != "EPERM" {
			t.Errorf("rename to %q: %s", p, got)
		}
		if got := h.call("path_link", preopen, 0, tp, tn, preopen, ptr, n); got != "EPERM" {
			t.Errorf("link to %q: %s", p, got)
		}
		if got := h.call("path_symlink", tp, tn, preopen, ptr, n); got != "EPERM" {
			t.Errorf("symlink at %q: %s", p, got)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); err == nil {
		t.Errorf("something was made outside the mount")
	}
	// A link within the mount works, and the link itself can be read.
	if fd, errno := h.open(preopen, "good", 0, rightRead, 0); errno != "ESUCCESS" {
		t.Errorf("open good: %s", errno)
	} else {
		h.call("fd_close", fd)
	}
	if _, errno := h.open(preopen, "good", 0, rightRead, 0); errno != "ESUCCESS" {
		t.Errorf("open good: %s", errno)
	}
	p, n := h.str(scratch, "good")
	h.expect("ELOOP", "path_open", preopen, 0, p, n, 0, rightRead, 0, 0, out)
	h.expect("ESUCCESS", "path_filestat_get", preopen, 0, p, n, out)
	if ft := h.bytes(out+16, 1)[0]; ft != 7 {
		t.Errorf("lstat type %d", ft)
	}
	h.expect("ESUCCESS", "path_readlink", preopen, p, n, out, 100, scratch+64)
	if got := string(h.bytes(out, h.u32(scratch+64))); got != "sub/f" {
		t.Errorf("readlink %q", got)
	}
	h.expect("ESUCCESS", "path_readlink", preopen, p, n, out, 3, scratch+64)
	if got := string(h.bytes(out, h.u32(scratch+64))); got != "sub" {
		t.Errorf("readlink into a short buffer %q", got)
	}
	// The readlink of an escaping link is its content: reading does not
	// follow it.
	p, n = h.str(scratch, "abs")
	h.expect("ESUCCESS", "path_readlink", preopen, p, n, out, 1000, scratch+64)
	p, n = h.str(scratch, "sub/f")
	h.expect("EINVAL", "path_readlink", preopen, p, n, out, 100, scratch+64)

	// Making links.
	tp, tn := h.str(scratch+256, "/etc/passwd")
	p, n = h.str(scratch, "new")
	h.expect("EPERM", "path_symlink", tp, tn, preopen, p, n)
	tp, tn = h.str(scratch+256, "sub/f")
	h.expect("ESUCCESS", "path_symlink", tp, tn, preopen, p, n)
	h.expect("EEXIST", "path_symlink", tp, tn, preopen, p, n)
	p, n = h.str(scratch, "hard")
	h.expect("ESUCCESS", "path_link", preopen, 0, tp, tn, preopen, p, n)
	h.expect("EEXIST", "path_link", preopen, 0, tp, tn, preopen, p, n)
	h.expect("EINVAL", "path_link", preopen, 1, tp, tn, preopen, p, n)
	if b, err := os.ReadFile(filepath.Join(dir, "hard")); err != nil || string(b) != "f" {
		t.Errorf("hard link %q, %v", b, err)
	}
	tp, tn = h.str(scratch+256, "sub")
	p, n = h.str(scratch, "subdup")
	h.expect("EPERM", "path_link", preopen, 0, tp, tn, preopen, p, n)
}

func TestReadOnlyFS(t *testing.T) {
	fsys := fstest.MapFS{
		"a.txt":     {Data: []byte("alpha")},
		"dir/b.txt": {Data: []byte("beta")},
	}
	h := newHarness(t, wasm.NewModuleConfig().WithFS(fsys))
	fd, errno := h.open(preopen, "a.txt", 0, rightRead, 0)
	if errno != "ESUCCESS" {
		t.Fatal(errno)
	}
	if errno, s := h.read(fd, 10); errno != "ESUCCESS" || s != "alpha" {
		t.Errorf("read %s %q", errno, s)
	}
	h.iovec(scratch, scratch+16, 3)
	h.expect("ESUCCESS", "fd_pread", fd, scratch, 1, 2, out)
	if got := string(h.bytes(scratch+16, h.u32(out))); got != "pha" {
		t.Errorf("pread %q", got)
	}
	if errno, _ := h.write(fd, "x"); errno != "EBADF" {
		t.Errorf("write %s", errno)
	}
	for _, oflags := range []uint64{oCreat, oTrunc} {
		if _, errno := h.open(preopen, "a.txt", oflags, rightRead, 0); errno != "EROFS" {
			t.Errorf("open with oflags %d: %s", oflags, errno)
		}
	}
	if _, errno := h.open(preopen, "a.txt", 0, rightWrite, 0); errno != "EROFS" {
		t.Errorf("open for writing: %s", errno)
	}
	if _, errno := h.open(preopen, "missing", 0, rightRead, 0); errno != "ENOENT" {
		t.Errorf("open a missing file: %s", errno)
	}
	p, n := h.str(scratch, "dir")
	h.expect("EROFS", "path_create_directory", preopen, p, n)
	h.expect("EROFS", "path_remove_directory", preopen, p, n)
	h.expect("EROFS", "path_unlink_file", preopen, p, n)
	h.expect("EINVAL", "path_readlink", preopen, p, n, out, 10, scratch+64)
	h.expect("ESUCCESS", "path_filestat_get", preopen, 0, p, n, out)
	if ft := h.bytes(out+16, 1)[0]; ft != 3 {
		t.Errorf("dir type %d", ft)
	}
	dfd, _ := h.open(preopen, "dir", oDirectory, rightRead, 0)
	h.expect("ESUCCESS", "fd_readdir", dfd, out, 1000, 0, scratch)
	if used := h.u32(scratch); used != 3*24+1+2+5 {
		t.Errorf("listing of %d bytes", used)
	}
	if _, errno := h.open(dfd, "../a.txt", 0, rightRead, 0); errno != "EPERM" {
		t.Errorf("open ../a.txt from dir: %s", errno)
	}
	if fd, errno := h.open(dfd, "b.txt", 0, rightRead, 0); errno != "ESUCCESS" {
		t.Errorf("open b.txt from dir: %s", errno)
	} else if errno, s := h.read(fd, 10); errno != "ESUCCESS" || s != "beta" {
		t.Errorf("read b.txt: %s %q", errno, s)
	}
}

func TestPollOneoff(t *testing.T) {
	var slept []int64
	cfg := wasm.NewModuleConfig().
		WithNanotime(func() int64 { return 1000 }, 1).
		WithNanosleep(func(ns int64) { slept = append(slept, ns) })
	h := newHarness(t, cfg)
	sub := func(i uint32, userdata uint64, typ byte, a uint32, timeout uint64, flags uint16) {
		b := make([]byte, 48)
		le.PutUint64(b, userdata)
		b[8] = typ
		le.PutUint32(b[16:], a)
		le.PutUint64(b[24:], timeout)
		le.PutUint16(b[40:], flags)
		h.mem.Write(scratch+48*i, b)
	}
	type event struct {
		userdata uint64
		errno    uint16
		typ      byte
		nbytes   uint64
	}
	events := func() (es []event) {
		for i := uint32(0); i < h.u32(out+0x1000); i++ {
			b := h.bytes(out+32*i, 32)
			es = append(es, event{le.Uint64(b), le.Uint16(b[8:]), b[10], le.Uint64(b[16:])})
		}
		return es
	}
	poll := func(n uint64) string { return h.call("poll_oneoff", scratch, out, n, out+0x1000) }

	// Two relative clocks: the earlier fires after a sleep.
	sub(0, 10, 0, 1, 500, 0)
	sub(1, 11, 0, 1, 200, 0)
	if errno := poll(2); errno != "ESUCCESS" {
		t.Fatal(errno)
	}
	if es := events(); len(es) != 1 || es[0] != (event{11, 0, 0, 0}) || len(slept) != 1 || slept[0] != 200 {
		t.Errorf("clocks: %v, slept %v", es, slept)
	}
	// An absolute clock in the past is ready without a sleep.
	slept = nil
	sub(0, 12, 0, 1, 900, 1)
	poll(1)
	if es := events(); len(es) != 1 || es[0].userdata != 12 || len(slept) != 0 {
		t.Errorf("past absolute clock: %v, slept %v", es, slept)
	}
	sub(0, 13, 0, 1, 1500, 1)
	poll(1)
	if len(slept) != 1 || slept[0] != 500 {
		t.Errorf("absolute clock slept %v", slept)
	}
	// Descriptors are ready at once, and no clock is waited for.
	slept = nil
	sub(0, 20, 0, 1, 1e9, 0)
	sub(1, 21, 1, 0, 0, 0)
	sub(2, 22, 2, 1, 0, 0)
	sub(3, 23, 1, 77, 0, 0)
	poll(4)
	es := events()
	want := []event{{21, 0, 1, 0}, {22, 0, 2, 0}, {23, 8, 1, 0}}
	if fmt.Sprint(es) != fmt.Sprint(want) || len(slept) != 0 {
		t.Errorf("descriptors: %v, want %v; slept %v", es, want, slept)
	}
	// Errors.
	sub(0, 30, 0, 9, 10, 0)
	poll(1)
	if es := events(); len(es) != 1 || es[0].errno != 28 {
		t.Errorf("unknown clock: %v", es)
	}
	sub(0, 31, 7, 0, 0, 0)
	if errno := poll(1); errno != "EINVAL" {
		t.Errorf("unknown event type: %s", errno)
	}
	if errno := poll(0); errno != "EINVAL" {
		t.Errorf("no subscriptions: %s", errno)
	}
}

func TestPollReadable(t *testing.T) {
	dir, h := mountDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("0123456789"), 0o644)
	fd, _ := h.open(preopen, "f", 0, rightRead, 0)
	h.read(fd, 4)
	b := make([]byte, 48)
	b[8] = 1
	le.PutUint32(b[16:], uint32(fd))
	h.mem.Write(scratch, b)
	h.expect("ESUCCESS", "poll_oneoff", scratch, out, 1, out+0x1000)
	if n := h.u64(out + 16); n != 6 {
		t.Errorf("nbytes %d, want 6", n)
	}
}

func TestRandom(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig().WithRandSource(bytes.NewReader([]byte{1, 2, 3})))
	h.expect("ESUCCESS", "random_get", out, 3)
	if got := h.bytes(out, 3); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("random %v", got)
	}
	h.expect("EIO", "random_get", out, 1)
	h.expect("ESUCCESS", "random_get", out, 0)
}

func TestProcExit(t *testing.T) {
	h := newHarness(t, wasm.NewModuleConfig())
	_, err := h.mod.ExportedFunction("proc_exit").Call(h.ctx, 7)
	var exit *sys.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("got %v, want exit code 7", err)
	}
	if !h.mod.IsClosed() {
		t.Errorf("the module is not closed")
	}
}

func TestErrnoName(t *testing.T) {
	for errno, name := range map[uint32]string{0: "ESUCCESS", 8: "EBADF", 44: "ENOENT", 76: "ENOTCAPABLE", 77: "errno(77)", 1 << 20: "errno(1048576)"} {
		if got := wasi.ErrnoName(errno); got != name {
			t.Errorf("ErrnoName(%d) = %q, want %q", errno, got, name)
		}
	}
}

func TestMissingMount(t *testing.T) {
	ctx := context.Background()
	r := wasm.NewRuntime(ctx)
	defer r.Close(ctx)
	wasi.MustInstantiate(ctx, r)
	bin, _ := wat.Compile([]byte("(module)"))
	cfg := wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(filepath.Join(t.TempDir(), "missing"), "/"))
	if _, err := r.InstantiateWithConfig(ctx, bin, cfg); err == nil {
		t.Errorf("instantiation with a missing directory succeeded")
	}
	// The name is free again.
	if _, err := r.InstantiateWithConfig(ctx, bin, wasm.NewModuleConfig().WithName("m")); err != nil {
		t.Error(err)
	}
}

func TestCloseClosesFiles(t *testing.T) {
	dir, h := mountDir(t)
	fd, errno := h.open(preopen, "f", oCreat, rightWrite, 0)
	if errno != "ESUCCESS" {
		t.Fatal(errno)
	}
	h.mod.Close(h.ctx)
	// The file is closed: on Windows it could not be removed otherwise.
	if err := os.Remove(filepath.Join(dir, "f")); err != nil {
		t.Error(err)
	}
	_ = fd
}
