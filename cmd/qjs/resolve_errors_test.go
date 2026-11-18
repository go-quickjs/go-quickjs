package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestResolutionErrorsNameTheImporter pins what a failed resolution says the
// request was made from, as node says it: the importing module's file, for
// an import, import.meta.resolve and a require's #imports; [eval] and [eval1]
// in the working directory for code given with -e, as CommonJS and as a
// module; and an "exports" error of an import, but not of a require, says
// so too.
func TestResolutionErrorsNameTheImporter(t *testing.T) {
	tree := map[string]string{
		"package.json":                 `{"name": "app", "imports": {"#a": "./a.js"}}`,
		"a.js":                         `module.exports = 1;`,
		"sub/dir/index.js":             `module.exports = 1;`,
		"node_modules/pm/package.json": `{"name": "pm", "exports": {"./a": "./a.js"}}`,
		"node_modules/pm/a.js":         `module.exports = 1;`,
		"sub/all.mjs": `const dir = import.meta.dirname.slice(0, -"/sub".length);
const norm = s => s.split(dir).join("<dir>").replaceAll("\\", "/");
for (const s of ["./nope.mjs", "nopkg", "./dir", "#b", "pm", "pm/x"]) {
  await import(s).then(() => console.log(s, "ok"), e => console.log(s, "|", e.code, "|", norm(e.message)));
}
try { import.meta.resolve("#b") } catch (e) { console.log("meta #b |", e.code, "|", norm(e.message)) }
`,
		"sub/c.cjs": `const dir = require("path").dirname(__dirname);
const norm = s => s.split(dir).join("<dir>").replaceAll("\\", "/");
for (const s of ["#b", "pm/x"]) {
  try { require(s) } catch (e) { console.log(s, "|", e.code, "|", norm(e.message.split("\n")[0])) }
}
`,
	}
	dir, err := filepath.EvalSymlinks(writeTree(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	from := " imported from <dir>/sub/all.mjs"
	for _, c := range []struct {
		name string
		args []string
		want []string
	}{
		{"import", []string{filepath.Join(dir, "sub", "all.mjs")}, []string{
			"./nope.mjs | ERR_MODULE_NOT_FOUND | Cannot find module '<dir>/sub/nope.mjs'" + from,
			"nopkg | ERR_MODULE_NOT_FOUND | Cannot find package 'nopkg'" + from,
			"./dir | ERR_UNSUPPORTED_DIR_IMPORT | Directory import '<dir>/sub/dir' is not supported resolving ES modules" + from,
			`#b | ERR_PACKAGE_IMPORT_NOT_DEFINED | Package import specifier "#b" is not defined in package <dir>/package.json` + from,
			`pm | ERR_PACKAGE_PATH_NOT_EXPORTED | No "exports" main defined in <dir>/node_modules/pm/package.json` + from,
			`pm/x | ERR_PACKAGE_PATH_NOT_EXPORTED | Package subpath './x' is not defined by "exports" in <dir>/node_modules/pm/package.json` + from,
			`meta #b | ERR_PACKAGE_IMPORT_NOT_DEFINED | Package import specifier "#b" is not defined in package <dir>/package.json` + from,
		}},
		{"require", []string{filepath.Join(dir, "sub", "c.cjs")}, []string{
			`#b | ERR_PACKAGE_IMPORT_NOT_DEFINED | Package import specifier "#b" is not defined in package <dir>/package.json imported from <dir>/sub/c.cjs`,
			`pm/x | ERR_PACKAGE_PATH_NOT_EXPORTED | Package subpath './x' is not defined by "exports" in <dir>/node_modules/pm/package.json`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := exec(t, "", c.args...)
			if want := strings.Join(c.want, "\n") + "\n"; code != 0 || out != want {
				t.Errorf("code=%d err=%q\ngot:\n%s\nwant:\n%s", code, errOut, out, want)
			}
		})
	}

	// Code given with -e resolves from the working directory, under the
	// name node gives it there.
	t.Chdir(dir)
	const eval = `import("./nope.mjs").catch(e => console.log(e.message.split(process.cwd()).join("<dir>").replaceAll("\\", "/")))`
	for _, c := range []struct {
		name string
		args []string
		want string
	}{
		{"eval", []string{"-e", eval}, "Cannot find module '<dir>/nope.mjs' imported from <dir>/[eval]\n"},
		{"eval module", []string{"-m", "-e", eval}, "Cannot find module '<dir>/nope.mjs' imported from <dir>/[eval1]\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, out, errOut := exec(t, "", c.args...)
			if code != 0 || out != c.want {
				t.Errorf("code=%d err=%q\ngot:  %q\nwant: %q", code, errOut, out, c.want)
			}
		})
	}
}
