package sysfs

import (
	"io"
	"sync"
)

// MaxFiles is the most descriptors a table holds at once.
const MaxFiles = 1 << 16

// Mount is a file system given to a module at a guest path: a host
// directory, read-only or not, or an fs.FS.
type Mount struct {
	// GuestPath is the name the module sees the mount by.
	GuestPath string
	// Dir is the host directory, unless FS is set.
	Dir      string
	ReadOnly bool
	// FS is the fs.FS-backed file system, mounted read-only.
	FS FS
}

// Entry is an open file descriptor.
type Entry struct {
	File File
	// FS and Path are the file system the file is in and its path there,
	// for a file of a mount; FS is nil for a standard stream.
	FS   FS
	Path string
	// IsDir reports a directory.
	IsDir bool
	// Preopen is the guest path of a mount's root directory, given to the
	// module at its start; empty for a file the module opened.
	Preopen string

	mu sync.Mutex
	// Flags are the descriptor's WASI fdflags.
	Flags uint16
	// RightsBase and RightsInheriting are the descriptor's WASI rights,
	// if RightsSet; otherwise they follow from its file's type.
	RightsBase, RightsInheriting uint64
	RightsSet                    bool
	// Restricted reports rights the module dropped, which are then
	// enforced.
	Restricted bool
	// dirents is the directory's listing, read when a module reads it from
	// its start.
	dirents []Dirent
}

// Lock and Unlock guard the entry's flags, rights and listing.
func (e *Entry) Lock()   { e.mu.Lock() }
func (e *Entry) Unlock() { e.mu.Unlock() }

// Dirents returns the directory's listing, reading it anew if fresh is true
// or it was never read. The caller holds the entry's lock.
func (e *Entry) Dirents(fresh bool) ([]Dirent, Errno) {
	if fresh || e.dirents == nil {
		ds, errno := e.File.Readdir()
		if errno != 0 {
			return nil, errno
		}
		if ds == nil {
			ds = []Dirent{}
		}
		e.dirents = ds
	}
	return e.dirents, 0
}

// Table is a module's file descriptors.
type Table struct {
	mu      sync.Mutex
	entries []*Entry
	mounts  []FS
	closed  bool
}

// NewTable returns the table of a module given standard streams and
// mounts: descriptors 0, 1 and 2 are stdin, stdout and stderr, and each
// mount's root directory follows, in order.
func NewTable(stdin io.Reader, stdout, stderr io.Writer, mounts []Mount) (*Table, error) {
	t := &Table{}
	t.entries = append(t.entries,
		&Entry{File: NewReaderFile(stdin)},
		&Entry{File: NewWriterFile(stdout)},
		&Entry{File: NewWriterFile(stderr)})
	for _, m := range mounts {
		fsys := m.FS
		if fsys == nil {
			var err error
			if fsys, err = OpenDir(m.Dir, m.ReadOnly); err != nil {
				t.Close()
				return nil, err
			}
		}
		t.mounts = append(t.mounts, fsys)
		f, errno := fsys.OpenFile(".", OpenFlags{Read: true, Directory: true})
		if errno != 0 {
			t.Close()
			return nil, errno
		}
		name := m.GuestPath
		t.entries = append(t.entries, &Entry{File: f, FS: fsys, Path: ".", IsDir: true, Preopen: name})
	}
	return t, nil
}

// Get returns the entry of descriptor fd.
func (t *Table) Get(fd uint32) (*Entry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if uint64(fd) >= uint64(len(t.entries)) || t.entries[fd] == nil {
		return nil, false
	}
	return t.entries[fd], true
}

// Insert adds e at the lowest free descriptor.
func (t *Table) Insert(e *Entry) (uint32, Errno) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, EBADF
	}
	for i, x := range t.entries {
		if x == nil {
			t.entries[i] = e
			return uint32(i), 0
		}
	}
	if len(t.entries) >= MaxFiles {
		return 0, EMFILE
	}
	t.entries = append(t.entries, e)
	return uint32(len(t.entries) - 1), 0
}

// CloseFD closes descriptor fd.
func (t *Table) CloseFD(fd uint32) Errno {
	t.mu.Lock()
	if uint64(fd) >= uint64(len(t.entries)) || t.entries[fd] == nil {
		t.mu.Unlock()
		return EBADF
	}
	e := t.entries[fd]
	t.entries[fd] = nil
	t.trim()
	t.mu.Unlock()
	return e.File.Close()
}

// trim drops the free descriptors at the table's end.
func (t *Table) trim() {
	n := len(t.entries)
	for n > 0 && t.entries[n-1] == nil {
		n--
	}
	clear(t.entries[n:])
	t.entries = t.entries[:n]
}

// Renumber moves descriptor from to to, closing what was at to.
func (t *Table) Renumber(from, to uint32) Errno {
	t.mu.Lock()
	n := uint64(len(t.entries))
	if uint64(from) >= n || t.entries[from] == nil || uint64(to) >= n || t.entries[to] == nil {
		t.mu.Unlock()
		return EBADF
	}
	if from == to {
		t.mu.Unlock()
		return 0
	}
	old := t.entries[to]
	t.entries[to] = t.entries[from]
	t.entries[from] = nil
	t.trim()
	t.mu.Unlock()
	old.File.Close()
	return 0
}

// Close closes every descriptor and mount.
func (t *Table) Close() error {
	t.mu.Lock()
	entries, mounts := t.entries, t.mounts
	t.entries, t.mounts, t.closed = nil, nil, true
	t.mu.Unlock()
	var err error
	for _, e := range entries {
		if e != nil {
			if errno := e.File.Close(); errno != 0 && err == nil {
				err = errno
			}
		}
	}
	for _, m := range mounts {
		if cerr := m.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
