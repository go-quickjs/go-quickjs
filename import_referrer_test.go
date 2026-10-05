package quickjs_test

import (
	"context"
	"path"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestDynamicImportReferrer pins what an import() hands the loader as its
// referrer: the module or script it is written in, as a static import hands
// its module -- also from a function called from elsewhere, after an await,
// in eval and Function code (their caller's), and for import.defer and
// import.source. It was always empty, so a loader resolving "./x" against
// the referrer resolved it against the working directory instead.
func TestDynamicImportReferrer(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	files := map[string]string{
		"/app/lib/a.mjs": `
			export const load = name => import("./" + name + ".mjs");
			export async function later() { await 0; return import("./t2.mjs") }
			export const direct = () => eval('import("./t3.mjs")');
			export const indirect = () => (0, eval)('import("./t4.mjs")');
			export const made = () => new Function('return import("./t5.mjs")')();
			export const deferred = () => import.defer("./t6.mjs");
			export const source = () => import.source("./t7.mjs");`,
	}
	var got []string
	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		got = append(got, specifier+" from "+referrer)
		resolved := specifier
		if strings.HasPrefix(specifier, ".") {
			base := "/cwd"
			if strings.HasPrefix(referrer, "/") {
				base = path.Dir(referrer)
			}
			resolved = path.Join(base, specifier)
		}
		src, ok := files[resolved]
		if !ok {
			src = `export const name = "` + resolved + `";`
		}
		return src, resolved, nil
	})

	ns, err := rt.EvalModule("/app/main.mjs", `
		import * as a from "./lib/a.mjs";
		export const all = Promise.all([
			a.load("t1"), a.later(), a.direct(), a.indirect(), a.made(), a.deferred(),
			a.source().catch(e => e.constructor.name), import("./own.mjs"),
		]);`)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := ns.Get("all")
	if _, err := all.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
	prog, err := rt.Compile("/srv/script.cjs", `import("./s.mjs")`)
	if err != nil {
		t.Fatal(err)
	}
	p, err := rt.RunProgram(prog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Await(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err = rt.Eval(`import("./e.mjs")`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Await(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"./lib/a.mjs from /app/main.mjs",
		"./t1.mjs from /app/lib/a.mjs",
		"./t3.mjs from /app/lib/a.mjs",
		"./t4.mjs from /app/lib/a.mjs",
		"./t5.mjs from /app/lib/a.mjs",
		"./t6.mjs from /app/lib/a.mjs",
		"./t7.mjs from /app/lib/a.mjs",
		"./own.mjs from /app/main.mjs",
		"./t2.mjs from /app/lib/a.mjs",
		"./s.mjs from /srv/script.cjs",
		"./e.mjs from ",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("loader calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
