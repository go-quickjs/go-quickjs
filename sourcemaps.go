package quickjs

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/sourcemap"
)

// SourceMapLoader reads the source map a script names with a
// //# sourceMappingURL= comment: script is the name the script was run
// under, and url what the comment says. It returns the map's JSON and the
// map's own URL, against which the sources the map names are resolved.
type SourceMapLoader func(script, url string) (data []byte, base string, err error)

// WithSourceMaps has an error's stack say where in the source each frame
// was before it was compiled -- the TypeScript a script was compiled from,
// say -- where the script names a source map with //# sourceMappingURL=,
// as Node's --enable-source-maps does: the frame is placed in the original
// file, at the original line and column, and called by the name the map
// gives it there.
//
// load reads the maps. With nil, only a map in the script itself, a data:
// URL, is read; ReadSourceMap reads them as Node does, from files too. A
// map is read the first time a stack in its script is written, once, and a
// script that cannot have its map read is written as it is. A trace a
// script's Error.prepareStackTrace makes gets the frames as they are, as
// Node's does.
func WithSourceMaps(load SourceMapLoader) Option {
	return func(c *config) {
		c.sourceMaps = true
		c.sourceMapLoader = load
	}
}

// installSourceMaps gives the runtime its loader.
func (r *Runtime) installSourceMaps(load SourceMapLoader) {
	r.rt.SetSourceMapLoader(func(script, u string) ([]byte, string, bool) {
		if load == nil {
			data, ok := sourcemap.DataURL(u)
			return data, sourcemap.URL(script), ok
		}
		data, base, err := load(script, u)
		return data, base, err == nil
	})
}

// ReadSourceMap is a SourceMapLoader that reads a map as Node does: from
// the script itself, a data: URL of JSON; or from the file a relative URL
// names, resolved against the script's file URL. It reads no map at an
// absolute URL other than a data: one, and none of a script in a
// node_modules directory, which Node maps only when told to.
func ReadSourceMap(script, u string) ([]byte, string, error) {
	slashed := filepath.ToSlash(script)
	if strings.Contains(slashed, "/node_modules/") {
		return nil, "", errors.New("quickjs: no source map for a script in node_modules")
	}
	scriptURL := sourcemap.URL(script)
	if parsed, err := url.Parse(u); err == nil && parsed.Scheme != "" && !isDrive(u) {
		if data, ok := sourcemap.DataURL(u); ok {
			return data, scriptURL, nil
		}
		return nil, "", errors.New("quickjs: a source map is read from a data: URL or a relative one")
	}
	base, err := url.Parse(scriptURL)
	if err != nil {
		return nil, "", err
	}
	ref, err := url.Parse(filepath.ToSlash(u))
	if err != nil {
		return nil, "", err
	}
	mapURL := base.ResolveReference(ref)
	if mapURL.Scheme != "file" {
		return nil, "", errors.New("quickjs: the source map is no file")
	}
	data, err := os.ReadFile(sourcemap.Path(mapURL.String()))
	if err != nil {
		return nil, "", err
	}
	return data, mapURL.String(), nil
}

// isDrive reports a Windows path, whose drive letter url.Parse takes for a
// scheme.
func isDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}
