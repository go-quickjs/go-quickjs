package sysfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// linkedDir makes a directory with symbolic links in it, some leading out
// of it, and mounts it.
func linkedDir(t *testing.T) (*dirFS, string, string) {
	t.Helper()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644)
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("f"), 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "abs")); err != nil {
		t.Skipf("no symbolic links: %v", err)
	}
	rel, err := filepath.Rel(dir, outside)
	if err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"rel":    rel,
		"good":   filepath.Join("sub", "f"),
		"dir":    "sub",
		"loop1":  "loop2",
		"loop2":  "loop1",
		"sub/up": filepath.Join("..", "..", "x"),
		"sub/in": filepath.Join("..", "sub", "f"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Fatal(err)
		}
	}
	fsys, err := OpenDir(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsys.Close() })
	return fsys.(*dirFS), dir, outside
}

// ops are the operations os.Root lacks, as the platform does them and as
// the resolution does them where the system has no *at calls.
type ops struct {
	rename, link func(from, to string) Errno
	symlink      func(target, p string) Errno
	readlink     func(p string) (string, Errno)
	utimes       func(p string, t Times, follow bool) Errno
}

func variants(d *dirFS) map[string]ops {
	return map[string]ops{
		"platform": {d.rename, d.link, d.symlink, d.readlink, d.utimes},
		"resolved": {d.resolvedRename, d.resolvedLink, d.resolvedSymlink, d.resolvedReadlink, d.resolvedUtimes},
	}
}

func TestDirFSLinksStayInside(t *testing.T) {
	d, dir, outside := linkedDir(t)
	for name, o := range variants(d) {
		t.Run(name, func(t *testing.T) {
			for _, p := range []string{"abs/x", "rel/x", "sub/up/x", "abs/../x"} {
				if e := o.rename("sub/f", p); e != EPERM {
					t.Errorf("rename to %s: %v", p, e)
				}
				if e := o.link("sub/f", p); e != EPERM {
					t.Errorf("link at %s: %v", p, e)
				}
				if e := o.symlink("sub/f", p); e != EPERM {
					t.Errorf("symlink at %s: %v", p, e)
				}
			}
			if e := o.rename("abs/secret", "stolen"); e != EPERM {
				t.Errorf("rename from outside: %v", e)
			}
			mt := Times{Mtim: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(), SetMtim: true}
			for _, p := range []string{"abs", "rel", "sub/up", "abs/secret"} {
				if e := o.utimes(p, mt, true); e != EPERM {
					t.Errorf("utimes of %s: %v", p, e)
				}
			}
			if e := o.utimes("loop1", mt, true); e != ELOOP {
				t.Errorf("utimes of a loop: %v", e)
			}
			if fi, err := os.Stat(filepath.Join(outside, "secret")); err != nil || fi.ModTime().UnixNano() == mt.Mtim {
				t.Errorf("the file outside was touched: %v", err)
			}
			if es, _ := os.ReadDir(outside); len(es) != 1 {
				t.Errorf("something was made outside: %v", es)
			}

			// Links within the directory are followed.
			if e := o.utimes("good", mt, true); e != 0 {
				t.Errorf("utimes of good: %v", e)
			}
			if fi, _ := os.Stat(filepath.Join(dir, "sub", "f")); fi.ModTime().UnixNano() != mt.Mtim {
				t.Errorf("utimes through good set %v", fi.ModTime())
			}
			if e := o.utimes("sub/in", Times{Mtim: 5e17, SetMtim: true}, true); e != 0 {
				t.Errorf("utimes of sub/in: %v", e)
			}
			if fi, _ := os.Stat(filepath.Join(dir, "sub", "f")); fi.ModTime().UnixNano() != 5e17 {
				t.Errorf("utimes through sub/in set %v", fi.ModTime())
			}
			if e := o.rename("sub/f", "dir/g"); e != 0 {
				t.Errorf("rename through dir: %v", e)
			}
			if e := o.link("dir/g", "dir/f"); e != 0 {
				t.Errorf("link through dir: %v", e)
			}
			if e := o.rename("dir/g", "gone"); e != 0 {
				t.Errorf("rename out of dir: %v", e)
			}
			os.Remove(filepath.Join(dir, "gone"))
			if s, e := o.readlink("dir/in"); e != 0 || s != "../sub/f" {
				t.Errorf("readlink dir/in: %q %v", s, e)
			}
			if e := o.symlink("f", "dir/s"); e != 0 {
				t.Errorf("symlink through dir: %v", e)
			}
			if s, e := o.readlink("sub/s"); e != 0 || s != "f" {
				t.Errorf("readlink sub/s: %q %v", s, e)
			}
			os.Remove(filepath.Join(dir, "sub", "s"))
		})
	}
}

func TestDirFSOpenStaysInside(t *testing.T) {
	d, _, _ := linkedDir(t)
	for _, p := range []string{"abs/secret", "rel/secret", "sub/up"} {
		if _, e := d.OpenFile(p, OpenFlags{Read: true}); e != EPERM {
			t.Errorf("open %s: %v", p, e)
		}
		if _, e := d.Stat(p, true); e != EPERM {
			t.Errorf("stat %s: %v", p, e)
		}
		if e := d.Mkdir(p + "/x"); e != EPERM {
			t.Errorf("mkdir under %s: %v", p, e)
		}
	}
	if _, e := d.OpenFile("good", OpenFlags{Read: true, NoFollow: true}); e != ELOOP {
		t.Errorf("open good without following: %v", e)
	}
	if _, e := d.OpenFile("good", OpenFlags{Create: true, Excl: true, Write: true}); e != EEXIST {
		t.Errorf("exclusive create of good: %v", e)
	}
	f, e := d.OpenFile("good", OpenFlags{Read: true})
	if e != 0 {
		t.Fatalf("open good: %v", e)
	}
	f.Close()
	if _, e := d.OpenFile("loop1", OpenFlags{Read: true}); e != ELOOP && e != EPERM {
		t.Errorf("open a loop: %v", e)
	}
}

func TestToErrno(t *testing.T) {
	_, err := os.Open(filepath.Join(t.TempDir(), "missing"))
	if e := ToErrno(err); e != ENOENT {
		t.Errorf("missing file: %v", e)
	}
	if e := ToErrno(nil); e != 0 {
		t.Errorf("nil: %v", e)
	}
	if ENOTCAPABLE.Name() != "ENOTCAPABLE" || Errno(200).Name() != "errno(200)" {
		t.Errorf("names")
	}
}
