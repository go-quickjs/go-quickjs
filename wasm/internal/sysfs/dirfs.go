package sysfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
)

// maxSymlinks is how many symbolic links a path may pass through.
const maxSymlinks = 40

// dirFS is a host directory mounted for a module, or a directory of a mount
// a module opened. Everything it does stays in the directory: an os.Root
// opens its files, and the operations os.Root lacks resolve their paths
// within it. The root is a handle of the directory, so that a directory a
// module opened is the one its paths resolve from wherever it is moved.
type dirFS struct {
	root     *os.Root
	dir      string // the directory's absolute host path when it was opened
	readOnly bool
	// mount is the mount's own dirFS: d itself for a mount.
	mount *dirFS
	// handle, for a directory a module opened on Windows, is a file of
	// it, which finds where it is now (base), paths being resolved by name
	// there.
	handle *os.File
	// refs counts what keeps a directory a module opened open: its
	// descriptor's file and the files opened from it. The last to close
	// closes its root.
	refs atomic.Int32
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
	d := &dirFS{root: root, dir: abs, readOnly: readOnly}
	d.mount = d
	return d, nil
}

func (d *dirFS) Close() error { return d.root.Close() }

// acquire and release count a use of a directory a module opened; a mount
// is closed with its table instead.
func (d *dirFS) acquire() {
	if d.mount != d {
		d.refs.Add(1)
	}
}

func (d *dirFS) release() {
	if d.mount != d && d.refs.Add(-1) == 0 {
		d.root.Close()
		closeIf(d.handle)
	}
}

// closeIf closes f if it is not nil.
func closeIf(f *os.File) {
	if f != nil {
		f.Close()
	}
}

// openDir opens the directory at p as a dirFS of its own, which resolves
// its paths from the directory itself, and returns a file of it, opened
// from the same handle, whose file system the dirFS is (DirFS).
func (d *dirFS) openDir(p string) (*osFile, Errno) {
	r, err := d.root.OpenRoot(p)
	if err != nil {
		return nil, ToErrno(err)
	}
	var h *os.File
	if runtime.GOOS == "windows" {
		// Only Windows resolves paths by name (base).
		if h, err = r.Open("."); err != nil {
			r.Close()
			return nil, ToErrno(err)
		}
	}
	f, err := r.Open(".")
	if err != nil {
		closeIf(h)
		r.Close()
		return nil, ToErrno(err)
	}
	of, e := newOSFile(f, true, false)
	if e != 0 {
		closeIf(h)
		r.Close()
		return nil, e
	}
	sub := &dirFS{root: r, dir: filepath.Join(d.dir, filepath.FromSlash(p)), readOnly: d.readOnly, mount: d.mount, handle: h}
	sub.refs.Store(1)
	d.acquire()
	of.sub, of.parent, of.name, of.readOnly = sub, d, p, d.readOnly
	return of, 0
}

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
	if of.isDir {
		// A directory is opened again as a root of its own.
		of.Close()
		return d.openDir(p)
	}
	d.acquire()
	of.parent, of.name, of.readOnly = d, p, d.readOnly
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

func (d *dirFS) Rename(from, to string) Errno { return d.renameTo(from, d, to) }

// renameTo renames from, in d, to to, in n, a directory of the same mount.
func (d *dirFS) renameTo(from string, n *dirFS, to string) Errno {
	if e := d.check(from); e != 0 {
		return e
	}
	if e := n.check(to); e != 0 {
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
		if nfi, err := n.root.Lstat(to); err == nil {
			switch {
			case os.SameFile(ofi, nfi):
				return 0
			case ofi.IsDir() && !nfi.IsDir():
				return ENOTDIR
			case !ofi.IsDir() && nfi.IsDir():
				return EISDIR
			case ofi.IsDir():
				if err := n.root.Remove(to); err != nil {
					return ToErrno(err)
				}
			}
		}
		if ofi.IsDir() && n == d && strings.HasPrefix(to+"/", from+"/") {
			return EINVAL
		}
	}
	return d.rename(from, n, to)
}

func (d *dirFS) Link(from, to string) Errno { return d.linkTo(from, d, to) }

// linkTo links from, in d, at to, in n, a directory of the same mount.
func (d *dirFS) linkTo(from string, n *dirFS, to string) Errno {
	if e := d.check(from); e != 0 {
		return e
	}
	if e := n.check(to); e != 0 {
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
	if _, err := n.root.Lstat(to); err == nil {
		return EEXIST
	}
	return d.link(from, n, to)
}

// Rename renames from, in ffs, to to, in tfs: the same file system, or
// directories of the same mount. Any other two fail with EXDEV.
func Rename(ffs FS, from string, tfs FS, to string) Errno {
	if ffs == tfs {
		return ffs.Rename(from, to)
	}
	d, ok := ffs.(*dirFS)
	n, ok2 := tfs.(*dirFS)
	if !ok || !ok2 || d.mount != n.mount {
		return EXDEV
	}
	return d.renameTo(from, n, to)
}

// Link makes a hard link at to, in tfs, of the file at from, in ffs, as
// Rename renames.
func Link(ffs FS, from string, tfs FS, to string) Errno {
	if ffs == tfs {
		return ffs.Link(from, to)
	}
	d, ok := ffs.(*dirFS)
	n, ok2 := tfs.(*dirFS)
	if !ok || !ok2 || d.mount != n.mount {
		return EXDEV
	}
	return d.linkTo(from, n, to)
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

// readdir lists the directory f, a file of d's own directory, looking each
// entry up through d's root. File.Readdir looks them up by the file's name,
// which for a root opened within another is, before Go 1.25, the path
// relative to that one: from the process's directory it names nothing,
// and Readdir drops every entry it cannot find.
func (d *dirFS) readdir(f *os.File) ([]Dirent, Errno) {
	names, err := f.Readdirnames(-1)
	if err != nil && err != io.EOF {
		return nil, ToErrno(err)
	}
	ds := make([]Dirent, 0, len(names))
	for _, name := range names {
		fi, err := d.root.Lstat(name)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed since it was listed
			}
			return nil, ToErrno(err)
		}
		ds = append(ds, direntOf(fi))
	}
	return ds, 0
}
