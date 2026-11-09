package wasi

import (
	"path"
	"strings"

	"github.com/go-quickjs/go-quickjs/wasm/internal/sysfs"
)

// lookupSymlinkFollow is the lookupflags bit that follows a symbolic link
// at the end of a path.
const lookupSymlinkFollow = 1

// The oflags.
const (
	oflagCreat     = 1 << 0
	oflagDirectory = 1 << 1
	oflagExcl      = 1 << 2
	oflagTrunc     = 1 << 3
)

// target is a path a module gave, resolved against the directory it is
// relative to.
type target struct {
	fs sysfs.FS
	// p is the path within fs: clean, and "." for its root.
	p string
	// slash reports a path that ended in a slash, which must name a
	// directory.
	slash bool
}

// path returns the path of len n at ptr, relative to directory fd, which
// needs right to be used so.
//
// The path is cleaned as a string: "a/../b" is "b" whether or not a is a
// symbolic link. A path that is absolute, or leads above the directory it
// is relative to, fails with EPERM.
func (c *call) path(fd uint64, ptr, n uint32, right uint64) (target, sysfs.Errno) {
	e, errno := c.file(fd)
	if errno != 0 {
		return target{}, errno
	}
	if !e.IsDir || e.FS == nil {
		return target{}, sysfs.ENOTDIR
	}
	if errno := need(e, right); errno != 0 {
		return target{}, errno
	}
	s, errno := c.str(ptr, n)
	if errno != 0 {
		return target{}, errno
	}
	switch {
	case s == "":
		return target{}, sysfs.ENOENT
	case strings.IndexByte(s, 0) >= 0:
		return target{}, sysfs.EINVAL
	case s[0] == '/':
		return target{}, sysfs.EPERM
	}
	clean := path.Clean(s)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return target{}, sysfs.EPERM
	}
	return target{fs: e.FS, p: path.Join(e.Path, clean), slash: strings.HasSuffix(s, "/")}, 0
}

// isDir reports whether t names a directory, following a symbolic link at
// its end, and the errno of finding out.
func (t target) isDir() (bool, sysfs.Errno) {
	st, errno := t.fs.Stat(t.p, true)
	if errno != 0 {
		return false, errno
	}
	return st.Mode.IsDir(), 0
}

// mustBeDir returns ENOTDIR for a path ending in a slash that names a file
// other than a directory.
func (t target) mustBeDir() sysfs.Errno {
	if !t.slash {
		return 0
	}
	if dir, errno := t.isDir(); errno == 0 && !dir {
		return sysfs.ENOTDIR
	}
	return 0
}

func pathCreateDirectory(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[1]), uint32(p[2]), rightPathCreateDirectory)
	if errno != 0 {
		return errno
	}
	return t.fs.Mkdir(t.p)
}

func pathFilestatGet(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[2]), uint32(p[3]), rightPathFilestatGet)
	if errno != 0 {
		return errno
	}
	follow := uint32(p[1])&lookupSymlinkFollow != 0 || t.slash
	st, errno := t.fs.Stat(t.p, follow)
	if errno != 0 {
		return errno
	}
	if t.slash && !st.Mode.IsDir() {
		return sysfs.ENOTDIR
	}
	return c.putFilestat(uint32(p[4]), &st)
}

func pathFilestatSetTimes(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[2]), uint32(p[3]), rightPathFilestatSetTimes)
	if errno != 0 {
		return errno
	}
	times, errno := c.times(p[4], p[5], uint32(p[6]))
	if errno != 0 {
		return errno
	}
	if errno := t.mustBeDir(); errno != 0 {
		return errno
	}
	follow := uint32(p[1])&lookupSymlinkFollow != 0 || t.slash
	if !times.SetAtim && !times.SetMtim {
		_, errno := t.fs.Stat(t.p, follow)
		return errno
	}
	return t.fs.Utimes(t.p, times, follow)
}

func pathLink(c *call, p []uint64) sysfs.Errno {
	from, errno := c.path(p[0], uint32(p[2]), uint32(p[3]), rightPathLinkSource)
	if errno != 0 {
		return errno
	}
	to, errno := c.path(p[4], uint32(p[5]), uint32(p[6]), rightPathLinkTarget)
	if errno != 0 {
		return errno
	}
	if from.fs != to.fs {
		return sysfs.EXDEV
	}
	if uint32(p[1]) != 0 {
		// A hard link is made of what the old path names, never of a
		// symbolic link's target.
		return sysfs.EINVAL
	}
	if from.slash || to.slash {
		dir, errno := from.isDir()
		switch {
		case errno != 0:
			return errno
		case dir:
			return sysfs.EPERM
		case from.slash:
			return sysfs.ENOTDIR
		}
		return sysfs.ENOENT
	}
	return from.fs.Link(from.p, to.p)
}

func pathOpen(c *call, p []uint64) sysfs.Errno {
	dirflags, oflags, base, fdflags := uint32(p[1]), uint32(p[4]), p[5], uint32(p[7])
	right := uint64(rightPathOpen)
	if oflags&oflagCreat != 0 {
		right |= rightPathCreateFile
	}
	if oflags&oflagTrunc != 0 {
		right |= rightPathFilestatSetSize
	}
	t, errno := c.path(p[0], uint32(p[2]), uint32(p[3]), right)
	if errno != 0 {
		return errno
	}
	if oflags&^(oflagCreat|oflagDirectory|oflagExcl|oflagTrunc) != 0 || fdflags&^fdflagsAll != 0 {
		return sysfs.EINVAL
	}
	fl := sysfs.OpenFlags{
		Create:    oflags&oflagCreat != 0,
		Excl:      oflags&oflagExcl != 0,
		Trunc:     oflags&oflagTrunc != 0,
		Directory: oflags&oflagDirectory != 0,
		NoFollow:  dirflags&lookupSymlinkFollow == 0,
		Read:      base&rightFdRead != 0,
		Write:     base&rightFdWrite != 0,
	}
	if fl.Directory && fl.Create {
		return sysfs.EINVAL
	}
	if !fl.Read && !fl.Write {
		// No rights for reading or writing: what the flags ask for.
		if fl.Create || fl.Trunc || fdflags&fdflagAppend != 0 {
			fl.Read, fl.Write = true, true
		} else {
			fl.Read = true
		}
	}
	if t.slash {
		dir, errno := t.isDir()
		switch {
		case errno == sysfs.ENOENT && fl.Create:
			return sysfs.EISDIR
		case errno == 0 && !dir:
			return sysfs.ENOTDIR
		}
		fl.Directory = true
	}
	f, errno := t.fs.OpenFile(t.p, fl)
	if errno != 0 {
		return errno
	}
	st, errno := f.Stat()
	if errno != 0 {
		f.Close()
		return errno
	}
	isDir := st.Mode.IsDir()
	if !isDir && fdflags&fdflagAppend != 0 {
		if errno := f.SetAppend(true); errno != 0 {
			f.Close()
			return errno
		}
	}
	rb, ri := defaultRights(filetype(st.Mode))
	if !isDir {
		if !fl.Read {
			rb &^= rightFdRead
		}
		if !fl.Write {
			rb &^= rightFdWrite | rightFdAllocate | rightFdFilestatSetSize
		}
	}
	e := &sysfs.Entry{
		File: f, FS: t.fs, Path: t.p, IsDir: isDir,
		Flags:      uint16(fdflags),
		RightsBase: rb, RightsInheriting: ri, RightsSet: true,
	}
	fd, errno := c.sc.Files.Insert(e)
	if errno != 0 {
		f.Close()
		return errno
	}
	if !c.putU32(uint32(p[8]), fd) {
		c.sc.Files.CloseFD(fd)
		return sysfs.EFAULT
	}
	return 0
}

func pathReadlink(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[1]), uint32(p[2]), rightPathReadlink)
	if errno != 0 {
		return errno
	}
	if t.slash {
		// The slash follows the link: what it names is not one.
		dir, errno := t.isDir()
		switch {
		case errno != 0:
			return errno
		case dir:
			return sysfs.EINVAL
		}
		return sysfs.ENOTDIR
	}
	s, errno := t.fs.Readlink(t.p)
	if errno != 0 {
		return errno
	}
	buf, ok := c.bytes(uint32(p[3]), uint32(p[4]))
	if !ok {
		return sysfs.EFAULT
	}
	n := copy(buf, s)
	if !c.putU32(uint32(p[5]), uint32(n)) {
		return sysfs.EFAULT
	}
	return 0
}

func pathRemoveDirectory(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[1]), uint32(p[2]), rightPathRemoveDirectory)
	if errno != 0 {
		return errno
	}
	return t.fs.Rmdir(t.p)
}

func pathRename(c *call, p []uint64) sysfs.Errno {
	from, errno := c.path(p[0], uint32(p[1]), uint32(p[2]), rightPathRenameSource)
	if errno != 0 {
		return errno
	}
	to, errno := c.path(p[3], uint32(p[4]), uint32(p[5]), rightPathRenameTarget)
	if errno != 0 {
		return errno
	}
	if from.fs != to.fs {
		return sysfs.EXDEV
	}
	if from.slash || to.slash {
		st, errno := from.fs.Stat(from.p, false)
		if errno != 0 {
			return errno
		}
		if !st.Mode.IsDir() {
			return sysfs.ENOTDIR
		}
	}
	return from.fs.Rename(from.p, to.p)
}

func pathSymlink(c *call, p []uint64) sysfs.Errno {
	old, errno := c.str(uint32(p[0]), uint32(p[1]))
	if errno != 0 {
		return errno
	}
	t, errno := c.path(p[2], uint32(p[3]), uint32(p[4]), rightPathSymlink)
	if errno != 0 {
		return errno
	}
	switch {
	case old == "":
		return sysfs.ENOENT
	case strings.IndexByte(old, 0) >= 0:
		return sysfs.EINVAL
	case old[0] == '/':
		return sysfs.EPERM
	}
	if t.slash {
		if _, errno := t.fs.Stat(t.p, false); errno == 0 {
			return sysfs.EEXIST
		}
		return sysfs.ENOENT
	}
	return t.fs.Symlink(old, t.p)
}

func pathUnlinkFile(c *call, p []uint64) sysfs.Errno {
	t, errno := c.path(p[0], uint32(p[1]), uint32(p[2]), rightPathUnlinkFile)
	if errno != 0 {
		return errno
	}
	if t.slash {
		dir, errno := t.isDir()
		switch {
		case errno != 0:
			return errno
		case dir:
			return sysfs.EISDIR
		}
		return sysfs.ENOTDIR
	}
	return t.fs.Unlink(t.p)
}
