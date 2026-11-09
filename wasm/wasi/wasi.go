// Package wasi implements WASI preview 1, the "wasi_snapshot_preview1"
// module, for the modules of a wasm.Runtime: their arguments, environment,
// standard streams, clocks, sleep, randomness and file systems, as each
// module's wasm.ModuleConfig gives them.
//
//	r := wasm.NewRuntime(ctx)
//	defer r.Close(ctx)
//	wasi.MustInstantiate(ctx, r)
//
//	cfg := wasm.NewModuleConfig().
//		WithArgs("prog", "-v").
//		WithStdout(os.Stdout).
//		WithFSConfig(wasm.NewFSConfig().WithDirMount("/srv/data", "/data"))
//	_, err := r.InstantiateWithConfig(ctx, binary, cfg)
//
// A program's exit, by proc_exit, closes its module and ends the call with
// a *sys.ExitError of the exit code; InstantiateModule returns it, or no
// error for exit code zero.
//
// # Files
//
// Descriptors 0, 1 and 2 are the standard streams, and the mounts the
// FSConfig gives follow in order, each a preopened directory named by its
// guest path. A path a module gives is relative to a directory descriptor
// and is cleaned as a string, "a/../b" being "b"; one that is absolute or
// leads above that directory fails with EPERM. Symbolic links are followed
// within the mount: an absolute one, or one whose target is outside the
// mount, fails with EPERM, and so does making an absolute one.
//
// What the host's file system answers is mapped to the WASI errno closest
// to it. Renaming and removing check the cases a host may answer
// differently -- a directory over a file, a file over a directory, a
// directory that is not empty -- so that a module sees the same errno on
// every platform.
//
// # Polling
//
// poll_oneoff waits on clocks, the real-time and monotonic clocks, by
// relative or absolute timeouts. A subscription to reading or writing a
// descriptor is ready at once, as reads and writes of files and of the
// streams never report EAGAIN; one of a descriptor that is not open is
// ready with EBADF. The call sleeps, with the module's Nanosleep, only when
// no subscription is ready: until the earliest clock.
//
// # What it does not do
//
// The clocks of process and thread CPU time are not given: clock_res_get
// and clock_time_get fail with EINVAL for them. proc_raise fails with
// ENOSYS. There are no sockets: the sock functions fail with EBADF for a
// descriptor that is not open and ENOTSOCK for one that is. Rights are
// reported, and enforced once a module drops some by fd_fdstat_set_rights:
// what needs a dropped right fails with ENOTCAPABLE. path_link links a
// symbolic link itself, and fails with EINVAL if asked to follow it.
package wasi

import (
	"context"
	"encoding/binary"
	"unicode/utf8"

	"github.com/go-quickjs/go-quickjs/wasm"
	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysctx"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
)

// ModuleName is the name modules import WASI preview 1 by.
const ModuleName = "wasi_snapshot_preview1"

// MustInstantiate instantiates WASI in r, panicking on an error.
func MustInstantiate(ctx context.Context, r wasm.Runtime) {
	if _, err := Instantiate(ctx, r); err != nil {
		panic(err)
	}
}

// Instantiate instantiates WASI in r, as ModuleName. Closing what it
// returns removes it from r.
func Instantiate(ctx context.Context, r wasm.Runtime) (api.Closer, error) {
	return NewBuilder(r).Instantiate(ctx)
}

// Builder makes the WASI host module.
type Builder interface {
	// Compile compiles the module, for r's InstantiateModule to
	// instantiate, under another name if its ModuleConfig gives one.
	Compile(ctx context.Context) (wasm.CompiledModule, error)

	// Instantiate instantiates the module as ModuleName.
	Instantiate(ctx context.Context) (api.Closer, error)
}

// NewBuilder returns a builder of WASI for r.
func NewBuilder(r wasm.Runtime) Builder { return &builder{r: r} }

type builder struct{ r wasm.Runtime }

func (b *builder) hostModule() wasm.HostModuleBuilder {
	hb := b.r.NewHostModuleBuilder(ModuleName)
	for _, f := range functions {
		fb := hb.NewFunctionBuilder().WithName(f.name).WithParameterNames(f.paramNames...)
		if f.exit {
			fb = fb.WithGoModuleFunction(api.GoModuleFunc(procExit), f.params, nil)
		} else {
			fb = fb.WithGoModuleFunction(f.goFunc(), f.params, []api.ValueType{i32}).WithResultNames("errno")
		}
		fb.Export(f.name)
	}
	return hb
}

func (b *builder) Compile(ctx context.Context) (wasm.CompiledModule, error) {
	return b.hostModule().Compile(ctx)
}

func (b *builder) Instantiate(ctx context.Context) (api.Closer, error) {
	return b.hostModule().Instantiate(ctx)
}

// ErrnoName returns the name of a WASI errno, such as "ENOENT", or
// "errno(N)" for a number WASI does not define.
func ErrnoName(errno uint32) string {
	if errno > 0xffff {
		return "errno(" + itoa(uint64(errno)) + ")"
	}
	return sysfs.Errno(errno).Name()
}

func itoa(n uint64) string {
	var b [20]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			return string(b[i:])
		}
	}
}

const (
	i32 = api.ValueTypeI32
	i64 = api.ValueTypeI64
)

var le = binary.LittleEndian

// function is a WASI function.
type function struct {
	name       string
	params     []api.ValueType
	paramNames []string
	impl       func(c *call, p []uint64) sysfs.Errno
	exit       bool // proc_exit, which returns nothing
}

func fn(name string, impl func(c *call, p []uint64) sysfs.Errno, params ...string) function {
	f := function{name: name, impl: impl}
	for _, p := range params {
		t := i32
		if p[0] == '$' {
			t, p = i64, p[1:]
		}
		f.params = append(f.params, t)
		f.paramNames = append(f.paramNames, p)
	}
	return f
}

func (f function) goFunc() api.GoModuleFunc {
	impl := f.impl
	return func(ctx context.Context, mod api.Module, stack []uint64) {
		sc := sysctx.FromModule(mod)
		if sc == nil {
			stack[0] = uint64(sysfs.ENOSYS)
			return
		}
		c := call{ctx: ctx, mod: mod, mem: mod.Memory(), sc: sc}
		stack[0] = uint64(impl(&c, stack))
	}
}

// functions are the module's functions, in the order of the
// specification's. A parameter name beginning with '$' is an i64.
var functions = []function{
	fn("args_get", argsGet, "argv", "argv_buf"),
	fn("args_sizes_get", argsSizesGet, "result.argc", "result.argv_len"),
	fn("environ_get", environGet, "environ", "environ_buf"),
	fn("environ_sizes_get", environSizesGet, "result.environc", "result.environv_len"),
	fn("clock_res_get", clockResGet, "id", "result.resolution"),
	fn("clock_time_get", clockTimeGet, "id", "$precision", "result.timestamp"),
	fn("fd_advise", fdAdvise, "fd", "$offset", "$len", "advice"),
	fn("fd_allocate", fdAllocate, "fd", "$offset", "$len"),
	fn("fd_close", fdClose, "fd"),
	fn("fd_datasync", fdDatasync, "fd"),
	fn("fd_fdstat_get", fdFdstatGet, "fd", "result.stat"),
	fn("fd_fdstat_set_flags", fdFdstatSetFlags, "fd", "flags"),
	fn("fd_fdstat_set_rights", fdFdstatSetRights, "fd", "$fs_rights_base", "$fs_rights_inheriting"),
	fn("fd_filestat_get", fdFilestatGet, "fd", "result.filestat"),
	fn("fd_filestat_set_size", fdFilestatSetSize, "fd", "$size"),
	fn("fd_filestat_set_times", fdFilestatSetTimes, "fd", "$atim", "$mtim", "fst_flags"),
	fn("fd_pread", fdPread, "fd", "iovs", "iovs_len", "$offset", "result.nread"),
	fn("fd_prestat_get", fdPrestatGet, "fd", "result.prestat"),
	fn("fd_prestat_dir_name", fdPrestatDirName, "fd", "path", "path_len"),
	fn("fd_pwrite", fdPwrite, "fd", "iovs", "iovs_len", "$offset", "result.nwritten"),
	fn("fd_read", fdRead, "fd", "iovs", "iovs_len", "result.nread"),
	fn("fd_readdir", fdReaddir, "fd", "buf", "buf_len", "$cookie", "result.bufused"),
	fn("fd_renumber", fdRenumber, "fd", "to"),
	fn("fd_seek", fdSeek, "fd", "$offset", "whence", "result.newoffset"),
	fn("fd_sync", fdSync, "fd"),
	fn("fd_tell", fdTell, "fd", "result.offset"),
	fn("fd_write", fdWrite, "fd", "iovs", "iovs_len", "result.nwritten"),
	fn("path_create_directory", pathCreateDirectory, "fd", "path", "path_len"),
	fn("path_filestat_get", pathFilestatGet, "fd", "flags", "path", "path_len", "result.filestat"),
	fn("path_filestat_set_times", pathFilestatSetTimes, "fd", "flags", "path", "path_len", "$atim", "$mtim", "fst_flags"),
	fn("path_link", pathLink, "old_fd", "old_flags", "old_path", "old_path_len", "new_fd", "new_path", "new_path_len"),
	fn("path_open", pathOpen, "fd", "dirflags", "path", "path_len", "oflags", "$fs_rights_base", "$fs_rights_inheriting", "fdflags", "result.opened_fd"),
	fn("path_readlink", pathReadlink, "fd", "path", "path_len", "buf", "buf_len", "result.bufused"),
	fn("path_remove_directory", pathRemoveDirectory, "fd", "path", "path_len"),
	fn("path_rename", pathRename, "fd", "old_path", "old_path_len", "new_fd", "new_path", "new_path_len"),
	fn("path_symlink", pathSymlink, "old_path", "old_path_len", "fd", "new_path", "new_path_len"),
	fn("path_unlink_file", pathUnlinkFile, "fd", "path", "path_len"),
	fn("poll_oneoff", pollOneoff, "in", "out", "nsubscriptions", "result.nevents"),
	{name: "proc_exit", params: []api.ValueType{i32}, paramNames: []string{"rval"}, exit: true},
	fn("proc_raise", procRaise, "sig"),
	fn("sched_yield", schedYield),
	fn("random_get", randomGet, "buf", "buf_len"),
	fn("sock_accept", sockAccept, "fd", "flags", "result.fd"),
	fn("sock_recv", sockRecv, "fd", "ri_data", "ri_data_len", "ri_flags", "result.ro_datalen", "result.ro_flags"),
	fn("sock_send", sockSend, "fd", "si_data", "si_data_len", "si_flags", "result.so_datalen"),
	fn("sock_shutdown", sockShutdown, "fd", "how"),
}

// call is a call of a WASI function: its caller and the caller's memory
// and system context.
type call struct {
	ctx context.Context
	mod api.Module
	mem api.Memory // nil for a module without memory
	sc  *sysctx.Context
}

// bytes returns the n bytes of memory at off.
func (c *call) bytes(off, n uint32) ([]byte, bool) {
	if c.mem == nil {
		return nil, n == 0
	}
	return c.mem.Read(off, n)
}

// array returns the count elements of size bytes at off.
func (c *call) array(off, count, size uint32) ([]byte, bool) {
	n := uint64(count) * uint64(size)
	if n > 0xffffffff {
		return nil, false
	}
	return c.bytes(off, uint32(n))
}

func (c *call) putU32(off, v uint32) bool {
	b, ok := c.bytes(off, 4)
	if ok {
		le.PutUint32(b, v)
	}
	return ok
}

func (c *call) putU64(off uint32, v uint64) bool {
	b, ok := c.bytes(off, 8)
	if ok {
		le.PutUint64(b, v)
	}
	return ok
}

// str returns the string of n bytes at off: EFAULT if it is out of range,
// EILSEQ if it is not UTF-8.
func (c *call) str(off, n uint32) (string, sysfs.Errno) {
	b, ok := c.bytes(off, n)
	if !ok {
		return "", sysfs.EFAULT
	}
	if !utf8.Valid(b) {
		return "", sysfs.EILSEQ
	}
	return string(b), 0
}

// file returns the entry of descriptor fd.
func (c *call) file(fd uint64) (*sysfs.Entry, sysfs.Errno) {
	e, ok := c.sc.Files.Get(uint32(fd))
	if !ok {
		return nil, sysfs.EBADF
	}
	return e, 0
}

// fileWith returns the entry of descriptor fd, which needs right for what
// the caller does.
func (c *call) fileWith(fd uint64, right uint64) (*sysfs.Entry, sysfs.Errno) {
	e, errno := c.file(fd)
	if errno != 0 {
		return nil, errno
	}
	if errno := need(e, right); errno != 0 {
		return nil, errno
	}
	return e, 0
}

// need returns ENOTCAPABLE if the module dropped any of rights from e's
// base rights.
func need(e *sysfs.Entry, rights uint64) sysfs.Errno {
	e.Lock()
	defer e.Unlock()
	if e.Restricted && e.RightsBase&rights != rights {
		return sysfs.ENOTCAPABLE
	}
	return 0
}
