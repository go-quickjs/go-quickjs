package sysfs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// maxSymlinks is how many symbolic links a path may pass through.
const maxSymlinks = 40

// dirFS is a host directory mounted for a module. Everything it does stays
// in the directory: an os.Root opens its files, and the operations os.Root
// lacks resolve their paths within it.
type dirFS struct {
	root     *os.Root
	dir      string // the directory's absolute host path
	readOnly bool
}

// OpenDir mounts host directory dir, read-only if readOnly is true.
func OpenDir(dir string, readOnly bool) (FS, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &dirFS{root: root, dir: abs, readOnly: readOnly}, nil
}

func (d *dirFS) Close() error { return d.root.Close() }

// check returns EPERM for a path that names something other than a file
// below the root: on Windows, a reserved device name such as NUL, a drive or
// a path a backslash makes escape.
func (d *dirFS) check(p string) Errno {
	if runtime.GOOS == "windows" && !filepath.IsLocal(filepath.FromSlash(p)) {
		return EPERM
	}
	return 0
}

func (d *dirFS) isSymlink(p string) bool {
	fi, err := d.root.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func (d *dirFS) OpenFile(p string, fl OpenFlags) (File, Errno) {
	if e := d.check(p); e != 0 {
		return nil, e
	}
	if d.readOnly && (fl.Write || fl.Create || fl.Trunc) {
		return nil, EROFS
	}
	if fl.Create && fl.Excl {
		if _, err := d.root.Lstat(p); err == nil {
			return nil, EEXIST
		}
	}
	if fl.NoFollow && d.isSymlink(p) {
		return nil, ELOOP
	}
	if fl.Write {
		if fi, err := d.root.Stat(p); err == nil && fi.IsDir() {
			return nil, EISDIR
		}
	}
	flag := os.O_RDONLY
	switch {
	case fl.Read && fl.Write:
		flag = os.O_RDWR
	case fl.Write:
		flag = os.O_WRONLY
	}
	if fl.Create {
		flag |= os.O_CREATE
	}
	if fl.Excl {
		flag |= os.O_EXCL
	}
	if fl.Trunc {
		flag |= os.O_TRUNC
	}
	f, err := d.root.OpenFile(p, flag, 0o666)
	if err != nil {
		return nil, ToErrno(err)
	}
	of, e := newOSFile(f, fl.Read || !fl.Write, fl.Write)
	if e != 0 {
		return nil, e
	}
	if fl.Directory && !of.isDir {
		of.Close()
		return nil, ENOTDIR
	}
	return of, 0
}

func (d *dirFS) Stat(p string, follow bool) (Stat, Errno) {
	if e := d.check(p); e != 0 {
		return Stat{}, e
	}
	var fi fs.FileInfo
	var err error
	if follow {
		fi, err = d.root.Stat(p)
	} else {
		fi, err = d.root.Lstat(p)
	}
	if err != nil {
		return Stat{}, ToErrno(err)
	}
	st := StatOf(fi)
	if pathIDs && fi.Mode()&fs.ModeSymlink == 0 {
		if f, err := d.root.Open(p); err == nil {
			fileIDs(f, &st)
			f.Close()
		}
	}
	return st, 0
}

func (d *dirFS) Mkdir(p string) Errno {
	if e := d.check(p); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	if p == "." {
		return EEXIST
	}
	return ToErrno(d.root.Mkdir(p, 0o777))
}

func (d *dirFS) Rmdir(p string) Errno {
	if e := d.check(p); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	if p == "." {
		return EBUSY
	}
	fi, err := d.root.Lstat(p)
	if err != nil {
		return ToErrno(err)
	}
	if !fi.IsDir() {
		return ENOTDIR
	}
	return ToErrno(d.root.Remove(p))
}

func (d *dirFS) Unlink(p string) Errno {
	if e := d.check(p); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	fi, err := d.root.Lstat(p)
	if err != nil {
		return ToErrno(err)
	}
	if fi.IsDir() {
		return EISDIR
	}
	return ToErrno(d.root.Remove(p))
}

func (d *dirFS) Rename(from, to string) Errno {
	if e := d.check(from); e != 0 {
		return e
	}
	if e := d.check(to); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	if from == "." || to == "." {
		return EBUSY
	}
	if runtime.GOOS == "windows" {
		// Windows renames a directory over nothing but a missing name, and
		// says little about why it would not: check each case, replacing
		// an empty directory as the rename would elsewhere.
		ofi, err := d.root.Lstat(from)
		if err != nil {
			return ToErrno(err)
		}
		if nfi, err := d.root.Lstat(to); err == nil {
			switch {
			case os.SameFile(ofi, nfi):
				return 0
			case ofi.IsDir() && !nfi.IsDir():
				return ENOTDIR
			case !ofi.IsDir() && nfi.IsDir():
				return EISDIR
			case ofi.IsDir():
				if err := d.root.Remove(to); err != nil {
					return ToErrno(err)
				}
			}
		}
		if ofi.IsDir() && strings.HasPrefix(to+"/", from+"/") {
			return EINVAL
		}
	}
	return d.rename(from, to)
}

func (d *dirFS) Link(from, to string) Errno {
	if e := d.check(from); e != 0 {
		return e
	}
	if e := d.check(to); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	fi, err := d.root.Lstat(from)
	if err != nil {
		return ToErrno(err)
	}
	if fi.IsDir() {
		return EPERM
	}
	if _, err := d.root.Lstat(to); err == nil {
		return EEXIST
	}
	return d.link(from, to)
}

func (d *dirFS) Symlink(target, p string) Errno {
	if e := d.check(p); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	if _, err := d.root.Lstat(p); err == nil {
		return EEXIST
	}
	return d.symlink(target, p)
}

func (d *dirFS) Readlink(p string) (string, Errno) {
	if e := d.check(p); e != 0 {
		return "", e
	}
	fi, err := d.root.Lstat(p)
	if err != nil {
		return "", ToErrno(err)
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return "", EINVAL
	}
	return d.readlink(p)
}

func (d *dirFS) Utimes(p string, t Times, follow bool) Errno {
	if e := d.check(p); e != 0 {
		return e
	}
	if d.readOnly {
		return EROFS
	}
	return d.utimes(p, t, follow)
}

// splitPath returns p's parent directory and last element.
func splitPath(p string) (dir, base string) {
	dir, base = path.Split(p)
	if dir == "" {
		dir = "."
	} else {
		dir = strings.TrimSuffix(dir, "/")
	}
	return dir, base
}

// tidy drops the empty and "." elements of p, keeping its ".." elements
// for the root to resolve where the elements before them lead.
func tidy(p string) string {
	var parts []string
	for _, e := range strings.Split(p, "/") {
		if e != "" && e != "." {
			parts = append(parts, e)
		}
	}
	if len(parts) == 0 {
		return "."
	}
	return strings.Join(parts, "/")
}
