//go:build !windows

package icu

func systemLocale() string { return "" }
