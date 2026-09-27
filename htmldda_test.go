package quickjs_test

import (
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// TestNewHTMLDDA pins what Annex B gives an object with [[IsHTMLDDA]]: typeof
// says "undefined", it is falsy, and it is == to null and undefined -- and
// nothing else treats it as anything but an object.
func TestNewHTMLDDA(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()

	calls := 0
	all, err := rt.NewHTMLDDA(func(args ...quickjs.Value) any {
		calls++
		if len(args) == 1 && args[0].IsString() {
			return "item " + args[0].String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := all.Set("count", 3); err != nil {
		t.Fatal(err)
	}
	if err := rt.Set("all", all); err != nil {
		t.Fatal(err)
	}
	if all.Bool() {
		t.Error("Value.Bool of document.all: true, want false")
	}

	for _, tc := range []struct{ src, want string }{
		{`typeof all`, "undefined"},
		{`[!all, !!all, all ? 1 : 2, (all && 1) === all, all || 2].join()`, "true,false,2,true,2"},
		{`[all == null, all == undefined, null == all, all != undefined].join()`, "true,true,true,false"},
		{`[all === undefined, all === null, Object.is(all, undefined), all == 0, all == ""].join()`, "false,false,false,false,false"},
		// ?? and ?. ask whether it is nullish, which it is not.
		{`[(all ?? 1) === all, all?.count].join()`, "true,3"},
		{`var {count} = all; var [x = 1] = [all]; [count, x === all].join()`, "3,true"},
		{`[all("a"), all()].join()`, "item a,"},
		{`[typeof Object(all), Array.isArray(all), all instanceof Function].join()`, "undefined,false,true"},
		// A proxy has no [[IsHTMLDDA]] of its own.
		{`typeof new Proxy(all, {})`, "function"},
	} {
		v, err := rt.Eval(tc.src)
		if err != nil {
			t.Errorf("%s: %v", tc.src, err)
		} else if got := v.String(); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.src, got, tc.want)
		}
	}
	if calls != 2 {
		t.Errorf("the Go function was called %d times, want 2", calls)
	}

	// Without a function it is not callable, and still undefined to typeof.
	plain, err := rt.NewHTMLDDA(nil)
	if err != nil {
		t.Fatal(err)
	}
	rt.Set("plain", plain)
	if v, err := rt.Eval(`var r; try { plain(); } catch (e) { r = e.name; } typeof plain + "," + r`); err != nil || v.String() != "undefined,TypeError" {
		t.Errorf("uncallable document.all = %v, %v; want undefined,TypeError", v, err)
	}

	// Anything but a Go function is refused: a Value may already be in a
	// script's hands.
	obj := rt.NewObject()
	if _, err := rt.NewHTMLDDA(obj); err == nil {
		t.Error("NewHTMLDDA(Value): no error")
	}
}

func TestValueIsString(t *testing.T) {
	rt := quickjs.New()
	defer rt.Close()
	v, err := rt.Eval(`["", "a", 1, null, new String("s"), Symbol()]`)
	if err != nil {
		t.Fatal(err)
	}
	want := []bool{true, true, false, false, false, false}
	for i, w := range want {
		el, err := v.Index(i)
		if err != nil {
			t.Fatal(err)
		}
		if got := el.IsString(); got != w {
			t.Errorf("element %d: IsString = %v, want %v", i, got, w)
		}
	}
}
