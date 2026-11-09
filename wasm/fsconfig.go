package wasm

import (
	"io/fs"

	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
)

// FSConfig says which file systems a module's system interfaces, such as
// WASI, give it, and at which guest paths. It is immutable: each With method
// returns a new FSConfig with the change.
//
// A guest path is the name the module sees a file system by: WASI gives
// each one as a preopened directory of that name. "", "." and "/" all name
// the root, which WASI calls "/"; any other path is given as it is written.
// Mounting at a guest path mounted before replaces that mount, keeping its
// place in the order the module sees them.
//
// Every mount is sandboxed: a path the module gives cannot leave it, by ".."
// or by a symbolic link, on any platform. Guest paths always use forward
// slashes, also on Windows.
type FSConfig interface {
	// WithDirMount mounts host directory dir at guestPath, for the module
	// to read and write. The directory is opened when a module is
	// instantiated, which fails if it cannot be.
	WithDirMount(dir, guestPath string) FSConfig

	// WithReadOnlyDirMount mounts host directory dir at guestPath, for the
	// module to read: what would change it fails with EROFS.
	WithReadOnlyDirMount(dir, guestPath string) FSConfig

	// WithFSMount mounts fsys at guestPath, read-only. A nil fsys removes
	// the mount at guestPath.
	WithFSMount(fsys fs.FS, guestPath string) FSConfig
}

type fsConfig struct {
	mounts []sysfs.Mount
}

// NewFSConfig returns a configuration with no file systems.
func NewFSConfig() FSConfig { return &fsConfig{} }

func (c *fsConfig) WithDirMount(dir, guestPath string) FSConfig {
	return c.with(sysfs.Mount{Dir: dir}, guestPath)
}

func (c *fsConfig) WithReadOnlyDirMount(dir, guestPath string) FSConfig {
	return c.with(sysfs.Mount{Dir: dir, ReadOnly: true}, guestPath)
}

func (c *fsConfig) WithFSMount(fsys fs.FS, guestPath string) FSConfig {
	if fsys == nil {
		return c.with(sysfs.Mount{}, guestPath)
	}
	return c.with(sysfs.Mount{FS: sysfs.AdaptFS(fsys)}, guestPath)
}

// with returns c with m mounted at guestPath, or the mount there removed if
// m is the zero Mount.
func (c *fsConfig) with(m sysfs.Mount, guestPath string) FSConfig {
	key := guestKey(guestPath)
	m.GuestPath = guestPath
	if key == "" {
		m.GuestPath = "/"
	}
	remove := m.FS == nil && m.Dir == ""
	r := &fsConfig{mounts: make([]sysfs.Mount, 0, len(c.mounts)+1)}
	replaced := false
	for _, x := range c.mounts {
		if guestKey(x.GuestPath) == key {
			replaced = true
			if remove {
				continue
			}
			x = m
		}
		r.mounts = append(r.mounts, x)
	}
	if !replaced && !remove {
		r.mounts = append(r.mounts, m)
	}
	return r
}

// guestKey returns guestPath without its leading "/" and "./" elements and
// its trailing slashes: the same key for each way of writing one path.
func guestKey(guestPath string) string {
	p := guestPath
	for len(p) > 0 && p[len(p)-1] == '/' {
		p = p[:len(p)-1]
	}
	for {
		switch {
		case len(p) > 0 && p[0] == '/':
			p = p[1:]
		case p == ".":
			p = ""
		case len(p) > 1 && p[0] == '.' && p[1] == '/':
			p = p[2:]
		default:
			return p
		}
	}
}

// WithFSConfig sets the file systems; the default is none. It replaces what
// WithFS set.
func (c *moduleConfig) WithFSConfig(config FSConfig) ModuleConfig {
	r := c.clone()
	r.sys.FS = nil
	r.sys.Mounts = nil
	if fc, ok := config.(*fsConfig); ok && fc != nil {
		r.sys.Mounts = fc.mounts
	}
	return r
}
