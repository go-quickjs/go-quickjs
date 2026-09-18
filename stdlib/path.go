package stdlib

import (
	"path"
	"strings"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Path installs the "path" module, which is pure computation: it works on the
// text of a path and never asks the filesystem anything, so a runtime with no
// filesystem at all can still use it.
//
// The separator is "/" whatever the host runs on, which is what a program that
// also runs in a browser or a container expects. A host that needs Windows
// paths should say so in its own module.
func Path(rt *quickjs.Runtime) error {
	exports := map[string]any{
		"sep":       "/",
		"delimiter": ":",

		"join": func(parts ...string) string {
			// An empty result is ".", as it is in node: joining nothing gives
			// the current directory rather than the root.
			kept := parts[:0]
			for _, p := range parts {
				if p != "" {
					kept = append(kept, p)
				}
			}
			if len(kept) == 0 {
				return "."
			}
			return path.Clean(strings.Join(kept, "/"))
		},
		"resolve": func(parts ...string) string {
			out := ""
			for _, p := range parts {
				if p == "" {
					continue
				}
				if strings.HasPrefix(p, "/") || out == "" {
					out = p
				} else {
					out += "/" + p
				}
			}
			if out == "" {
				return "/"
			}
			if !strings.HasPrefix(out, "/") {
				// Without a working directory to resolve against, a relative
				// path resolves against the root: a runtime has no ambient
				// notion of where it is unless the host gave it one.
				out = "/" + out
			}
			return path.Clean(out)
		},
		"normalize": func(p string) string {
			if p == "" {
				return "."
			}
			clean := path.Clean(p)
			if strings.HasSuffix(p, "/") && !strings.HasSuffix(clean, "/") {
				clean += "/"
			}
			return clean
		},
		"basename": func(p string, ext quickjs.Value) string {
			base := path.Base(p)
			if base == "/" || base == "." {
				return base
			}
			if ext.Kind() == quickjs.KindString {
				if suffix := ext.String(); suffix != "" && suffix != base &&
					strings.HasSuffix(base, suffix) {
					base = base[:len(base)-len(suffix)]
				}
			}
			return base
		},
		"dirname":    path.Dir,
		"extname":    path.Ext,
		"isAbsolute": func(p string) bool { return strings.HasPrefix(p, "/") },
		"relative": func(from, to string) string {
			return relativePath(from, to)
		},
	}
	exports["default"] = copyExports(exports)
	if err := rt.SetModule("path", exports); err != nil {
		return err
	}
	return rt.SetModule("node:path", exports)
}

// copyExports is the default export: the same names, in an object.
//
// A module that is imported both ways -- `import path from "path"` and
// `import {join} from "path"` -- has to offer both, and the default cannot be
// the namespace itself, which is not an ordinary object.
func copyExports(exports map[string]any) map[string]any {
	out := make(map[string]any, len(exports))
	for k, v := range exports {
		if k == "default" {
			continue
		}
		out[k] = v
	}
	return out
}

// relativePath is the path from one directory to another, in terms of the text
// alone.
func relativePath(from, to string) string {
	from, to = path.Clean("/"+from), path.Clean("/"+to)
	if from == to {
		return ""
	}
	fromParts := strings.Split(strings.TrimPrefix(from, "/"), "/")
	toParts := strings.Split(strings.TrimPrefix(to, "/"), "/")
	i := 0
	for i < len(fromParts) && i < len(toParts) && fromParts[i] == toParts[i] {
		i++
	}
	var out []string
	for j := i; j < len(fromParts); j++ {
		if fromParts[j] != "" {
			out = append(out, "..")
		}
	}
	for j := i; j < len(toParts); j++ {
		if toParts[j] != "" {
			out = append(out, toParts[j])
		}
	}
	return strings.Join(out, "/")
}
