package vm

import "testing"

// TestCallerMadeWhenAsked pins what a sloppy function's own caller costs in
// standards mode: nothing until caller itself is asked about or the
// function's keys are listed. Reading, defining or describing its name and
// length leaves caller out of the table and the prototype unbuilt; asking
// about caller puts it in without building the prototype; and the realm's
// functions share one getter and one setter.
func TestCallerMadeWhenAsked(t *testing.T) {
	src := `
		function f() {}
		function g() {}
		function h() {}
		f.name; f.length;
		Object.defineProperty(f, "name", {value: "renamed"});
		Object.getOwnPropertyDescriptor(f, "length");
		f.hasOwnProperty("name");
		g.caller;
		Reflect.ownKeys(h);
		[f, g, h]`
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	fns := v.Object().elemsOf()
	f, g, h := fns[0].Object(), fns[1].Object(), fns[2].Object()
	if fd := f.fn(); fd.callerMade || !fd.protoPending {
		t.Errorf("f: callerMade %v, protoPending %v after its name and length; want false, true",
			fd.callerMade, fd.protoPending)
	}
	if fd := g.fn(); !fd.callerMade || !fd.protoPending {
		t.Errorf("g: callerMade %v, protoPending %v after its caller; want true, true",
			fd.callerMade, fd.protoPending)
	}
	if fd := h.fn(); !fd.callerMade {
		t.Errorf("h: caller not made by listing its keys")
	}
	gc, hc := g.getOwn(atomCaller), h.getOwn(atomCaller)
	if !gc.ok() || !hc.ok() {
		t.Fatalf("caller in the tables: g %v, h %v", gc.ok(), hc.ok())
	}
	ga, ha := gc.getterSetter(), hc.getterSetter()
	if ga.getter != ha.getter || ga.setter != ha.setter || ga.getter == nil || ga.setter == nil {
		t.Errorf("the realm's functions do not share caller's getter and setter")
	}
}
