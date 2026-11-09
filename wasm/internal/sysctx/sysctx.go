// Package sysctx holds what a module was configured with for system
// interfaces -- arguments, environment, standard streams, file system,
// clocks, sleep and randomness -- for the host functions of this module's
// packages, such as a WASI implementation, to read.
//
// A host function finds its caller's with FromModule:
//
//	func(ctx context.Context, mod api.Module, stack []uint64) {
//		sc := sysctx.FromModule(mod)
//		sc.Stdout.Write(...)
//	}
package sysctx

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm/api"
	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
	"github.com/go-quickjs/go-quickjs/wasm/sys"
)

// Context is a module's system configuration. Its fields are set once, at
// instantiation, and never change; what they refer to -- streams, clocks --
// may be used by several modules at once.
type Context struct {
	// Args are the arguments, argv[0] first if the embedder gave one.
	Args []string
	// Environ are the environment's entries, each "key=value", in the order
	// the keys were first set.
	Environ []string

	// Stdin reads io.EOF unless configured; Stdout and Stderr discard
	// unless configured.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// FS is the file system, or nil for none.
	FS fs.FS

	// Files are the module's file descriptors: the standard streams, then
	// the mounts' root directories, then what the module opens.
	Files *sysfs.Table

	// Walltime and Nanotime are the clocks. Unless configured they are
	// fakes that advance by a millisecond on every reading: the wall clock
	// from 2022-01-01T00:00:00Z, the monotonic one from zero.
	Walltime           sys.Walltime
	WalltimeResolution sys.ClockResolution
	Nanotime           sys.Nanotime
	NanotimeResolution sys.ClockResolution
	// Nanosleep and Osyield return at once unless configured.
	Nanosleep sys.Nanosleep
	Osyield   sys.Osyield

	// RandSource reads random bytes: a fixed, deterministic sequence
	// unless configured.
	RandSource io.Reader
}

// Config is what a ModuleConfig gives New.
type Config struct {
	Args       []string
	EnvKeys    []string
	EnvValues  []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	FS         fs.FS
	Mounts     []sysfs.Mount
	Walltime   sys.Walltime
	WalltimeRe sys.ClockResolution
	Nanotime   sys.Nanotime
	NanotimeRe sys.ClockResolution
	Nanosleep  sys.Nanosleep
	Osyield    sys.Osyield
	RandSource io.Reader
}

// fakeEpoch is where the fake wall clock starts: 2022-01-01T00:00:00Z.
const fakeEpoch = 1640995200

// New returns the context of c, filling in the defaults, or an error for an
// argument or environment entry that a C program could not see: one holding
// a NUL, or an environment key that is empty or holds '=', or for a mounted
// directory that cannot be opened. The context holds the mounts open until
// it is closed.
func New(c *Config) (*Context, error) {
	for _, a := range c.Args {
		if strings.IndexByte(a, 0) >= 0 {
			return nil, errors.New("args invalid: contains NUL character")
		}
	}
	sc := &Context{
		Args:       c.Args,
		Stdin:      c.Stdin,
		Stdout:     c.Stdout,
		Stderr:     c.Stderr,
		FS:         c.FS,
		Walltime:   c.Walltime,
		Nanotime:   c.Nanotime,
		Nanosleep:  c.Nanosleep,
		Osyield:    c.Osyield,
		RandSource: c.RandSource,

		WalltimeResolution: c.WalltimeRe,
		NanotimeResolution: c.NanotimeRe,
	}
	for i, k := range c.EnvKeys {
		v := c.EnvValues[i]
		switch {
		case k == "":
			return nil, errors.New("environ invalid: empty key")
		case strings.IndexByte(k, '=') >= 0:
			return nil, errors.New("environ invalid: key contains '=' character")
		case strings.IndexByte(k, 0) >= 0 || strings.IndexByte(v, 0) >= 0:
			return nil, errors.New("environ invalid: contains NUL character")
		}
		sc.Environ = append(sc.Environ, k+"="+v)
	}
	if sc.Stdin == nil {
		sc.Stdin = eofReader{}
	}
	if sc.Stdout == nil {
		sc.Stdout = io.Discard
	}
	if sc.Stderr == nil {
		sc.Stderr = io.Discard
	}
	files, err := sysfs.NewTable(sc.Stdin, sc.Stdout, sc.Stderr, c.Mounts)
	if err != nil {
		return nil, err
	}
	sc.Files = files
	if sc.Walltime == nil {
		var t atomic.Int64
		sc.Walltime = func() (int64, int32) {
			n := t.Add(int64(time.Millisecond))
			return fakeEpoch + n/1e9, int32(n % 1e9)
		}
		sc.WalltimeResolution = sys.ClockResolution(time.Microsecond)
	}
	if sc.Nanotime == nil {
		var t atomic.Int64
		sc.Nanotime = func() int64 {
			return t.Add(int64(time.Millisecond))
		}
		sc.NanotimeResolution = 1
	}
	if sc.WalltimeResolution == 0 {
		sc.WalltimeResolution = 1
	}
	if sc.NanotimeResolution == 0 {
		sc.NanotimeResolution = 1
	}
	if sc.Nanosleep == nil {
		sc.Nanosleep = func(int64) {}
	}
	if sc.Osyield == nil {
		sc.Osyield = func() {}
	}
	if sc.RandSource == nil {
		sc.RandSource = rand.NewChaCha8([32]byte{'g', 'o', '-', 'q', 'u', 'i', 'c', 'k', 'j', 's'})
	}
	return sc, nil
}

// Close closes the module's files and mounts.
func (c *Context) Close() error { return c.Files.Close() }

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// Provider is implemented by the modules the wasm package makes. Only this
// module's packages can name it.
type Provider interface {
	SysContext() *Context
}

// FromModule returns mod's system context, or nil for a module that has
// none: a host module, or nil.
func FromModule(mod api.Module) *Context {
	if p, ok := mod.(Provider); ok {
		return p.SysContext()
	}
	return nil
}
