//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package sysfs

func (d *dirFS) rename(from string, n *dirFS, to string) Errno {
	return d.resolvedRename(from, n, to)
}

func (d *dirFS) link(from string, n *dirFS, to string) Errno { return d.resolvedLink(from, n, to) }

func (d *dirFS) symlink(target, p string) Errno { return d.resolvedSymlink(target, p) }

func (d *dirFS) readlink(p string) (string, Errno) { return d.resolvedReadlink(p) }

func (d *dirFS) utimes(p string, t Times, follow bool) Errno {
	return d.resolvedUtimes(p, t, follow)
}
