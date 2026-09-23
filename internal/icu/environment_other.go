//go:build !windows

package icu

func systemLocale() string { return "" }

// systemZone is Windows-only: a Unix machine says which zone it is set to
// through TZ or /etc/localtime, which the caller reads directly.
func systemZone() string { return "" }
