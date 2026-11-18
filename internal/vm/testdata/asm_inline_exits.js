// Typed callees that leave native code where they are inlined, for the
// JIT's test of exits inside inlined typed callees
// (TestAsmJITInlinedExits): a call of an import, its result converted --
// a string's number, an object's valueOf, an oddball's, a throw -- two
// callees deep; a callee's loop, which polls; a double's % and pow, which
// Go computes where they are not exact; native calls from an inlined
// callee, a recursion's and one past the stack's limit; and module
// variables and the heap written in a callee.
function InlineExits(stdlib, foreign, heap) {
    "use asm";
    var imp = foreign.imp;
    var pow = stdlib.Math.pow;
    var H32 = new stdlib.Int32Array(heap);
    var g = 0;
    var gd = 0.0;

    function conv(x) {
        x = x | 0;
        return imp(x | 0) | 0;
    }

    function convF(x) {
        x = x | 0;
        return +imp(x | 0);
    }

    function viaConv(x) {
        x = x | 0;
        return ((conv(x) | 0) + 1) | 0;
    }

    function outerConv(n) {
        n = n | 0;
        var i = 0, s = 0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            s = (s + (viaConv(i) | 0)) | 0;
        return s | 0;
    }

    function outerConvF(n) {
        n = n | 0;
        var i = 0, s = 0.0, d = 0.0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
            d = +convF(i);
            if (d == d)
                s = s + d;
        }
        return +s;
    }

    function sumTo(n) {
        n = n | 0;
        var i = 0, s = 0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            s = (s + i) | 0;
        return s | 0;
    }

    function outerSum(n) {
        n = n | 0;
        var i = 0, s = 0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            s = (s + (sumTo(i & 15) | 0)) | 0;
        return s | 0;
    }

    function dmod(a, b) {
        a = +a;
        b = +b;
        return +(a % b);
    }

    function dpow(a, b) {
        a = +a;
        b = +b;
        return +pow(a, b);
    }

    function outerD(n) {
        n = n | 0;
        var i = 0, s = 0.0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            s = s + +dmod(+(i | 0) * 1.5, 7.25) + +dpow(1.5, +(i & 7)) + +dpow(+(i | 0), 0.5);
        return +s;
    }

    function fib(k) {
        k = k | 0;
        if ((k | 0) < 2)
            return k | 0;
        return ((fib((k - 1) | 0) | 0) + (fib((k - 2) | 0) | 0)) | 0;
    }

    function fibw(k) {
        k = k | 0;
        return fib(k) | 0;
    }

    function deep(n) {
        n = n | 0;
        if ((n | 0) == 0)
            return 0;
        return ((deep((n - 1) | 0) | 0) + 1) | 0;
    }

    function deepw(n) {
        n = n | 0;
        return deep(n) | 0;
    }

    function setG(x, d) {
        x = x | 0;
        d = +d;
        g = (g + x) | 0;
        gd = gd + d;
        H32[(x & 1023) << 2 >> 2] = g;
    }

    function outerG(n) {
        n = n | 0;
        var i = 0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0)
            setG(i, +(i | 0) * 0.25);
        return +(+(g | 0) + gd + +(H32[7] | 0));
    }

    return { outerConv: outerConv, outerConvF: outerConvF, outerSum: outerSum, outerD: outerD,
             fibw: fibw, deepw: deepw, outerG: outerG };
}

var inlineCalls = 0, inlineStacks = [], inlineThrowAt = -1;

// What the import gives for x: a number, a string, an object whose valueOf
// counts its calls, an oddball, or, at inlineThrowAt, a throw; at 37 it
// keeps the stack its error has.
function inlineImp(x) {
    if (x === inlineThrowAt)
        throw new TypeError("thrown at " + x);
    if (x === 37)
        inlineStacks.push(new Error("at " + x).stack);
    switch (x % 6) {
    case 0: return x;
    case 1: return String(x * 2);
    case 2: return { valueOf: function () { inlineCalls++; return x * 3; } };
    case 3: return x & 1 ? true : null;
    case 4: return undefined;
    }
    return x + 0.75;
}

var inlineExitsModule = InlineExits(globalThis, { imp: inlineImp }, new ArrayBuffer(65536));

// The answers, which Node gives (asm_inline_exits.want).
function inlineExits() {
    var m = inlineExitsModule, out = [];
    inlineThrowAt = -1;
    out.push("conv: " + [m.outerConv(10), m.outerConv(200), m.outerConv(3000)].join(","));
    out.push("convF: " + [m.outerConvF(10), m.outerConvF(200), m.outerConvF(3000)].join(","));
    out.push("valueOf calls: " + inlineCalls);
    var thrown = [];
    for (var at = 0; at < 6; at++) {
        inlineThrowAt = 100 + at;
        try { m.outerConv(1000); thrown.push("none"); } catch (e) { thrown.push(e.message); }
    }
    inlineThrowAt = -1;
    out.push("thrown: " + thrown.join(","));
    out.push("sum: " + [m.outerSum(10), m.outerSum(1000), m.outerSum(100000)].join(","));
    out.push("d: " + [m.outerD(10), m.outerD(1000), m.outerD(20000)].join(","));
    out.push("fib: " + [m.fibw(1), m.fibw(10), m.fibw(25)].join(","));
    var deep = m.deepw(1000);
    try { deep += "," + m.deepw(1e7); } catch (e) { deep += "," + (e instanceof RangeError); }
    out.push("deep: " + deep);
    out.push("g: " + [m.outerG(10), m.outerG(1000), m.outerG(100000)].join(","));
    return out.join("\n");
}

// The stacks of errors made and thrown inside the inlined callees, which
// the typed interpreter and the ordinary code give.
function inlineExitStacks() {
    var m = inlineExitsModule, out = [];
    inlineStacks = [];
    inlineThrowAt = -1;
    m.outerConv(40);
    out.push(inlineStacks.join("\n"));
    inlineThrowAt = 23;
    try { m.outerConv(40); } catch (e) { out.push(e.stack); }
    inlineThrowAt = -1;
    try { m.deepw(1e7); } catch (e) { out.push(e instanceof RangeError, e.stack); }
    return out.join("\n");
}
