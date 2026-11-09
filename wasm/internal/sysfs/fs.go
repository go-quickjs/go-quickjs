// Package sysfs is the file system the system interfaces give a module:
// the directories and fs.FS trees mounted for it, the files it opens, and
// its table of file descriptors.
//
// Paths given to an FS are relative to its root, slash-separated and clean:
// "." for the root, never "..", a leading or trailing slash, or an empty
// element. Callers clean what a module gives them before passing it on; an
// FS keeps symbolic links from leading out of its root.
package sysfs

import "io/fs"

// Stat describes a file.
type Stat struct {
	Dev, Ino uint64
	Mode     fs.FileMode
	Nlink    uint64
	Size     int64
	// Atim, Mtim and Ctim are times in nanoseconds since the Unix epoch.
	Atim, Mtim, Ctim int64
}

// Dirent is a directory entry.
type Dirent struct {
	Name string
	Ino  uint64
	Type fs.FileMode // the type bits of the entry's mode
}

// OpenFlags say how to open a file.
type OpenFlags struct {
	Read, Write bool
	// Create creates the file if it does not exist; with Excl, it must not
	// exist.
	Create, Excl bool
	// Trunc truncates a regular file to zero length.
	Trunc bool
	// Directory requires the file to be a directory.
	Directory bool
	// NoFollow fails with ELOOP if the path names a symbolic link.
	NoFollow bool
}

// Times are the times to set on a file: each nanoseconds since the Unix
// epoch, unless its Set is false, which leaves it as it is.
type Times struct {
	Atim, Mtim       int64
	SetAtim, SetMtim bool
}

// FS is a file system: a mounted directory or fs.FS. Its methods may be
// called from several goroutines at once.
type FS interface {
	// OpenFile opens the file at p.
	OpenFile(p string, flags OpenFlags) (File, Errno)
	// Stat describes the file at p, following a symbolic link at p if
	// follow is true.
	Stat(p string, follow bool) (Stat, Errno)
	// Mkdir makes a directory.
	Mkdir(p string) Errno
	// Rmdir removes an empty directory.
	Rmdir(p string) Errno
	// Unlink removes a file that is not a directory.
	Unlink(p string) Errno
	// Rename renames from to to, replacing a file or empty directory
	// there.
	Rename(from, to string) Errno
	// Link makes a hard link at to of the file at from.
	Link(from, to string) Errno
	// Symlink makes a symbolic link at p whose content is target.
	Symlink(target, p string) Errno
	// Readlink returns the content of the symbolic link at p.
	Readlink(p string) (string, Errno)
	// Utimes sets the access and modification times of the file at p,
	// following a symbolic link at p if follow is true.
	Utimes(p string, t Times, follow bool) Errno
	// Close releases the file system. Files opened from it may still be
	// used and closed.
	Close() error
}

// File is an open file. Its methods may be called from several goroutines
// at once.
type File interface {
	// Read reads at the file's offset, returning 0 and no error at the end.
	Read(p []byte) (int, Errno)
	// Pread reads at off without moving the offset.
	Pread(p []byte, off int64) (int, Errno)
	// Write writes at the file's offset, or at its end in append mode.
	Write(p []byte) (int, Errno)
	// Pwrite writes at off without moving the offset, in append mode too.
	Pwrite(p []byte, off int64) (int, Errno)
	// Lseek moves the offset as io.Seeker does.
	Lseek(off int64, whence int) (int64, Errno)
	// Stat describes the file.
	Stat() (Stat, Errno)
	// Readdir lists a directory from its start, without "." and "..".
	Readdir() ([]Dirent, Errno)
	// Truncate sets the size of a regular file.
	Truncate(size int64) Errno
	// Sync flushes what was written to storage.
	Sync() Errno
	// SetAppend sets append mode, in which every write goes to the end.
	SetAppend(on bool) Errno
	// Close closes the file.
	Close() Errno
}
