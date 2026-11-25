package wasi_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/go-quickjs/go-quickjs/wasm"
)

// TestDirectoryDescriptorFollowsItsDirectory checks that a directory
// descriptor names the directory it was opened on, not the path it was
// opened by: after the directory is renamed -- by the host, or by the
// module -- and another made in its place, what the descriptor's paths
// name is in the directory it holds.
func TestDirectoryDescriptorFollowsItsDirectory(t *testing.T) {
	dir, h := mountDir(t)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	dfd, errno := h.open(preopen, "d", oDirectory, ^uint64(rightWrite), 0)
	if errno != "ESUCCESS" {
		t.Fatalf("open d: %s", errno)
	}
	// The host renames d and makes another d.
	if err := os.Rename(filepath.Join(dir, "d"), filepath.Join(dir, "moved")); err != nil {
		t.Fatalf("renaming the directory a descriptor holds: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fd, errno := h.open(dfd, "made", oCreat|oExcl, rightRead|rightWrite, 0)
	if errno != "ESUCCESS" {
		t.Fatalf("create through the descriptor: %s", errno)
	}
	h.write(fd, "x")
	h.call("fd_close", fd)
	p, n := h.str(scratch, "sub")
	h.expect("ESUCCESS", "path_create_directory", dfd, p, n)
	if _, err := os.Stat(filepath.Join(dir, "moved", "made")); err != nil {
		t.Errorf("the file is not in the descriptor's directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "moved", "sub")); err != nil {
		t.Errorf("the directory is not in the descriptor's directory: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "d")); len(ents) != 0 {
		t.Errorf("the directory now named d has %d entries; want none", len(ents))
	}
	// The module renames it, through another descriptor.
	from, fn := h.str(scratch, "moved")
	to, tn := h.str(scratch+16, "again")
	h.expect("ESUCCESS", "path_rename", preopen, from, fn, preopen, to, tn)
	p, n = h.str(scratch, "made")
	h.expect("ESUCCESS", "path_filestat_get", dfd, 0, p, n, out)
	if size := h.u64(out + 32); size != 1 {
		t.Errorf("made through the descriptor after a rename: size %d, want 1", size)
	}
	// A path still cannot lead above the descriptor's mount.
	p, n = h.str(scratch, "../../x")
	h.expect("EPERM", "path_create_directory", dfd, p, n)
}

// TestFdFilestatSetTimesSetsTheFile checks that fd_filestat_set_times sets
// the times of the file a descriptor holds, not of whatever has its name
// since.
func TestFdFilestatSetTimesSetsTheFile(t *testing.T) {
	dir, h := mountDir(t)
	os.WriteFile(filepath.Join(dir, "f"), []byte("old"), 0o644)
	fd, errno := h.open(preopen, "f", 0, rightRead, 0)
	if errno != "ESUCCESS" {
		t.Fatalf("open f: %s", errno)
	}
	if err := os.Rename(filepath.Join(dir, "f"), filepath.Join(dir, "g")); err != nil {
		t.Fatalf("renaming an open file: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "f"), []byte("new"), 0o644)
	const mtim = 1_000_000_000_123_456_700 // in 100 ns, as NTFS keeps times
	switch runtime.GOOS {
	case "darwin", "freebsd", "netbsd", "openbsd":
		// A regular file's times are set by its name there, while that
		// names it still (docs/wasm.md's WASI gaps): no file's change.
		h.expect("ENOENT", "fd_filestat_set_times", fd, 0, mtim, 4)
		for _, name := range []string{"f", "g"} {
			if fi, _ := os.Stat(filepath.Join(dir, name)); fi.ModTime().Equal(time.Unix(0, mtim)) {
				t.Errorf("%s had its times set", name)
			}
		}
		h.call("fd_close", fd)
		return
	}
	h.expect("ESUCCESS", "fd_filestat_set_times", fd, 0, mtim, 4)
	if fi, err := os.Stat(filepath.Join(dir, "g")); err != nil || !fi.ModTime().Equal(time.Unix(0, mtim)) {
		t.Errorf("the descriptor's file's mtime: %v, %v; want %v", fi.ModTime(), err, time.Unix(0, mtim))
	}
	if fi, _ := os.Stat(filepath.Join(dir, "f")); fi.ModTime().Equal(time.Unix(0, mtim)) {
		t.Errorf("the file now named f had its times set")
	}
	h.call("fd_close", fd)
}

// TestDirectoryDescriptorLinksStayInIt checks that a symbolic link is
// followed within the directory a path is relative to: from a directory a
// module opened, a link leading above that directory fails with EPERM,
// even to a directory of the same mount, which the mount's own descriptor
// reaches by the same link.
func TestDirectoryDescriptorLinksStayInIt(t *testing.T) {
	dir, h := mountDir(t)
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.MkdirAll(filepath.Join(dir, "t"), 0o755)
	if err := os.Symlink(filepath.Join("..", "t"), filepath.Join(dir, "d", "up")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	dfd, errno := h.open(preopen, "d", oDirectory, ^uint64(rightWrite), 0)
	if errno != "ESUCCESS" {
		t.Fatalf("open d: %s", errno)
	}
	if _, errno := h.open(dfd, "up/x", oCreat, rightRead|rightWrite, 0); errno != "EPERM" {
		t.Errorf("create through a link above the descriptor's directory: %s, want EPERM", errno)
	}
	if fd, errno := h.open(preopen, "d/up/x", oCreat, rightRead|rightWrite, 0); errno != "ESUCCESS" {
		t.Errorf("create through the link from the mount: %s", errno)
	} else {
		h.call("fd_close", fd)
	}
}

// TestRenameBetweenDirectoryDescriptors checks that a rename or a link
// between two directories a module opened of one mount is made, and one
// between two mounts fails with EXDEV.
func TestRenameBetweenDirectoryDescriptors(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	h := newHarness(t, wasm.NewModuleConfig().WithFSConfig(wasm.NewFSConfig().WithDirMount(dir, "/").WithDirMount(other, "/o")))
	os.MkdirAll(filepath.Join(dir, "a"), 0o755)
	os.MkdirAll(filepath.Join(dir, "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "f"), []byte("f"), 0o644)
	afd, _ := h.open(preopen, "a", oDirectory, ^uint64(rightWrite), 0)
	bfd, _ := h.open(preopen, "b", oDirectory, ^uint64(rightWrite), 0)
	from, fn := h.str(scratch, "f")
	to, tn := h.str(scratch+16, "g")
	h.expect("ESUCCESS", "path_rename", afd, from, fn, bfd, to, tn)
	if _, err := os.Stat(filepath.Join(dir, "b", "g")); err != nil {
		t.Errorf("rename between descriptors: %v", err)
	}
	from, fn = h.str(scratch, "g")
	to, tn = h.str(scratch+16, "h")
	h.expect("ESUCCESS", "path_link", bfd, 0, from, fn, afd, to, tn)
	if _, err := os.Stat(filepath.Join(dir, "a", "h")); err != nil {
		t.Errorf("link between descriptors: %v", err)
	}
	h.expect("EXDEV", "path_rename", afd, to, tn, preopen+1, from, fn)
}
