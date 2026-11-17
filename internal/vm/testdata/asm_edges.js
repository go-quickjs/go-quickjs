// asm.js modules over the edges of their types, whose results the typed
// code must give exactly as the same source run as plain JavaScript does:
// zlib's kind of int32 arithmetic and heap, doubles and floats, the
// conversions, Math, calls through tables and imports, and a heap
// detached by an import. TestAsmEdges compares every engine and tier.
var asmLog = [];

function AsmEdges(stdlib, foreign, heap) {
    "use asm";
    var H8 = new stdlib.Int8Array(heap);
    var U8 = new stdlib.Uint8Array(heap);
    var H16 = new stdlib.Int16Array(heap);
    var U16 = new stdlib.Uint16Array(heap);
    var H32 = new stdlib.Int32Array(heap);
    var U32 = new stdlib.Uint32Array(heap);
    var F32 = new stdlib.Float32Array(heap);
    var F64 = new stdlib.Float64Array(heap);
    var imul = stdlib.Math.imul;
    var fround = stdlib.Math.fround;
    var floor = stdlib.Math.floor;
    var ceil = stdlib.Math.ceil;
    var sqrt = stdlib.Math.sqrt;
    var abs = stdlib.Math.abs;
    var pow = stdlib.Math.pow;
    var atan2 = stdlib.Math.atan2;
    var min = stdlib.Math.min;
    var max = stdlib.Math.max;
    var clz32 = stdlib.Math.clz32;
    var sin = stdlib.Math.sin;
    var exp = stdlib.Math.exp;
    var log = stdlib.Math.log;
    var PI = stdlib.Math.PI;
    var inf = stdlib.Infinity;
    var nan = stdlib.NaN;
    var ffiInt = foreign.ffiInt;
    var ffiDouble = foreign.ffiDouble;
    var ffiVoid = foreign.ffiVoid;
    var start = foreign.start | 0;
    var scale = +foreign.scale;
    var gi = 0;
    var gd = 0.5;
    var gf = fround(1.5);

    function add(a, b) {
        a = a | 0;
        b = b | 0;
        return (a + b) | 0;
    }

    function wrap(a, b) {
        a = a | 0;
        b = b | 0;
        var s = 0;
        s = (a + b + 7) | 0;
        s = (s - (a * 3 | 0) + (b << 4)) | 0;
        return (s ^ imul(a, b)) | 0;
    }

    function divs(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) / (b | 0)) | 0;
    }

    function rems(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) % (b | 0)) | 0;
    }

    function divu(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) / (b >>> 0)) | 0;
    }

    function remu(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) % (b >>> 0)) | 0;
    }

    function shifts(a, n) {
        a = a | 0;
        n = n | 0;
        return ((a << n) ^ (a >> n) ^ (a >>> n) ^ (a >>> 0 > 1000 ? 1 : 2)) | 0;
    }

    function ucmp(a, b) {
        a = a | 0;
        b = b | 0;
        var r = 0;
        if ((a >>> 0) < (b >>> 0)) r = 1;
        if ((a | 0) < (b | 0)) r = r | 2;
        if ((a >>> 0) <= (b >>> 0)) r = r | 4;
        if ((a | 0) >= (b | 0)) r = r | 8;
        return r | 0;
    }

    function trunc(x) {
        x = +x;
        return ~~x;
    }

    function dbl(x, y) {
        x = +x;
        y = +y;
        var r = 0.0;
        r = x * y + x / y - x % y;
        if (r < x) r = r + 1.0;
        if (!(r <= y)) r = -r;
        return +r;
    }

    function negzero(x) {
        x = +x;
        return +(-x * 0.0);
    }

    function fcmp(x, y) {
        x = +x;
        y = +y;
        var r = 0;
        if (x < y) r = 1;
        if (x <= y) r = r | 2;
        if (x == y) r = r | 4;
        if (x != y) r = r | 8;
        if (x > y) r = r | 16;
        if (x >= y) r = r | 32;
        r = r | ((x < y) << 6) | ((x == y) << 7);
        return r | 0;
    }

    function flt(x, y) {
        x = fround(x);
        y = fround(y);
        var z = fround(0);
        z = fround(x * y);
        z = fround(z + fround(0.1));
        z = fround(z / fround(3));
        return fround(z);
    }

    function maths(x) {
        x = +x;
        return +(floor(x) + ceil(x) * 2.0 + sqrt(abs(x)) + pow(x, 2.0) + atan2(x, 1.5) +
            min(x, 1.0, 2.0) + max(x, -1.0) + sin(x) + exp(x / 10.0) + log(abs(x) + 1.0) + PI);
    }

    function imaths(a, b) {
        a = a | 0;
        b = b | 0;
        return (clz32(a) + (abs(a | 0) | 0) + min(a | 0, b | 0) + max(a | 0, b | 0)) | 0;
    }

    function conv(a) {
        a = a | 0;
        var d = 0.0, u = 0.0;
        d = +(a | 0);
        u = +(a >>> 0);
        return +(d * 2.0 + u);
    }

    function heapw(p) {
        p = p | 0;
        H8[p] = 300;
        U8[p + 1 | 0] = -2;
        H16[(p + 2) >> 1] = 70000;
        U16[(p + 4) >> 1] = -3;
        H32[(p + 8) >> 2] = 4294967295;
        F32[(p + 12) >> 2] = 0.1;
        F64[(p + 16) >> 3] = -0.25;
    }

    function heapr(p) {
        p = p | 0;
        return ((H8[p] | 0) + (U8[p] | 0) + (H16[p >> 1] | 0) + (U16[p >> 1] | 0) + (H32[p >> 2] | 0) +
            (U32[p >> 2] >>> 0) + ~~+F32[(p + 12) >> 2] + ~~(+F64[(p + 16) >> 3] * 100.0)) | 0;
    }

    function heapf(p) {
        p = p | 0;
        return +(+F32[(p + 12) >> 2] + +F64[(p + 16) >> 3]);
    }

    function oob(p) {
        p = p | 0;
        var r = 0;
        r = ((H32[p >> 2] | 0) + (U8[p] | 0) + (H16[p >> 1] | 0)) | 0;
        H32[p >> 2] = 12345;
        U8[p] = 7;
        return r | 0;
    }

    function oobf(p) {
        p = p | 0;
        return +(+F64[p >> 3] + +F32[p >> 2]);
    }

    function globals(n) {
        n = n | 0;
        gi = (gi + n) | 0;
        gd = gd * 1.5 + scale;
        gf = fround(gf * fround(2));
        start = (start + 1) | 0;
        return +(+(gi | 0) + gd + +gf + +(start | 0));
    }

    function calls(n) {
        n = n | 0;
        var i = 0, s = 0, d = 0.0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
            s = (s + (ops[i & 3](s, i) | 0)) | 0;
            s = (s + (ffiInt(i | 0, +(s | 0)) | 0)) | 0;
            d = d + +ffiDouble(s | 0);
            ffiVoid(i | 0);
        }
        return +(d + +(s | 0));
    }

    function loops(n) {
        n = n | 0;
        var i = 0, j = 0, s = 0;
        outer: for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
            j = 0;
            while (1) {
                j = (j + 1) | 0;
                if ((j | 0) > (i | 0)) continue outer;
                if ((j & 7) == 5) break;
                s = (s + j) | 0;
            }
            do {
                s = (s ^ i) | 0;
                switch (s & 3) {
                case 0:
                    s = (s + 1) | 0;
                    break;
                case 1:
                case 2:
                    s = (s + 3) | 0;
                default:
                    s = (s - 1) | 0;
                }
            } while ((s & 1) != 0);
        }
        return s | 0;
    }

    function cond(a) {
        a = a | 0;
        return ((a ? 3 : 4) + ((a | 0) > 5 ? a : (0 - a) | 0) + (!a)) | 0;
    }

    function fib(k) {
        k = k | 0;
        if ((k | 0) < 2) return k | 0;
        return ((fib((k - 1) | 0) | 0) + (fib((k - 2) | 0) | 0)) | 0;
    }

    function deep(k) {
        k = k | 0;
        if ((k | 0) == 0) return 0;
        return ((deep((k - 1) | 0) | 0) + 1) | 0;
    }

    function op0(a, b) { a = a | 0; b = b | 0; return (a + b) | 0; }
    function op1(a, b) { a = a | 0; b = b | 0; return (a - b) | 0; }
    function op2(a, b) { a = a | 0; b = b | 0; return (a ^ b) | 0; }
    function op3(a, b) { a = a | 0; b = b | 0; return imul(a, b) | 0; }

    var ops = [op0, op1, op2, op3];

    return { add: add, wrap: wrap, divs: divs, rems: rems, divu: divu, remu: remu, shifts: shifts, ucmp: ucmp,
             trunc: trunc, dbl: dbl, negzero: negzero, fcmp: fcmp, flt: flt, maths: maths, imaths: imaths,
             conv: conv, heapw: heapw, heapr: heapr, heapf: heapf, oob: oob, oobf: oobf, globals: globals,
             calls: calls, loops: loops, cond: cond, fib: fib, deep: deep };
}

// A module that says "use asm" but is not asm.js: it runs as plain code.
function NotAsm(stdlib, foreign, heap) {
    "use asm";
    function f(a) {
        a = a | 0;
        return ("x" + a).length | 0;
    }
    return { f: f };
}

// NaNs of every payload in the heap, the tags' among them, read by typed
// code and handed out every way a raw float leaves it: a return, an
// import's argument, a module variable, a copy read from script.
function AsmNaNs(stdlib, foreign, heap) {
    "use asm";
    var F64 = new stdlib.Float64Array(heap);
    var F32 = new stdlib.Float32Array(heap);
    var see = foreign.see;
    var g = 0.0;
    function ret(p) { p = p | 0; return +F64[p >> 3]; }
    function arg(p) { p = p | 0; see(+F64[p >> 3]); }
    function glob(p) { p = p | 0; g = +F64[p >> 3]; return +(g + 0.0); }
    function f32(p) { p = p | 0; return +F32[p >> 2]; }
    function copy(p, q) { p = p | 0; q = q | 0; F64[q >> 3] = F64[p >> 3]; }
    return { ret: ret, arg: arg, glob: glob, f32: f32, copy: copy };
}

function asmNaNs() {
    var out = [];
    var buf = new ArrayBuffer(0x1000);
    var u = new Uint32Array(buf);
    var m = AsmNaNs(globalThis, { see: function (x) { out.push("see " + typeof x + " " + (x !== x)); } }, buf);
    var hi = [0x7FF80000, 0x7FF00001, 0xFFF80000, 0xFFF90000, 0xFFFA0000, 0xFFFC0000, 0xFFFE0000, 0xFFFFFFFF, 0x7FFFFFFF];
    for (var i = 0; i < hi.length; i++) {
        u[2 * i] = 0x12345678;
        u[2 * i + 1] = hi[i];
        var r = m.ret(8 * i), gl = m.glob(8 * i), f = m.f32(8 * i + 4);
        m.arg(8 * i);
        m.copy(8 * i, 0x800 + 8 * i);
        var back = new Float64Array(buf, 0x800 + 8 * i, 1)[0];
        // The copy's bits are the engine's to choose: a NaN stored into a
        // Float64Array is any NaN (ES's SetValueInBuffer).
        out.push([typeof r, r !== r, typeof gl, gl !== gl, typeof f, f !== f, typeof back, back !== back].join(" "));
    }
    return out.join("\n");
}

function asmEdges() {
    var out = [];
    var buf = new ArrayBuffer(0x10000);
    var throwAt = -1;
    var foreign = {
        ffiInt: function (i, d) {
            if (i == throwAt) throw new TypeError("ffi " + i);
            if (i == 3) return { valueOf: function () { return 77; } };
            return i * 2 + (d > 0 ? 1 : 0);
        },
        ffiDouble: function (s) { return s / 4; },
        ffiVoid: function (i) { asmLog.push(i); },
        start: { valueOf: function () { return 40; } },
        scale: "0.25"
    };
    var m = AsmEdges(globalThis, foreign, buf);
    globalThis.asmEdgesModule = m;
    var I = [0, 1, -1, 2, 3, 7, 100, -100, 65535, 65536, 2147483647, -2147483648, 1073741824, -1073741825];
    var D = [0, -0, 0.5, -1.5, 1e10, -1e10, 3e9, -3e9, 2147483648.5, 1e300, NaN, Infinity, -Infinity, 0.1];
    var i, j, k;
    for (i = 0; i < I.length; i++) {
        for (j = 0; j < I.length; j++) {
            var a = I[i], b = I[j];
            out.push([m.add(a, b), m.wrap(a, b), m.divs(a, b), m.rems(a, b), m.divu(a, b), m.remu(a, b),
                      m.shifts(a, b & 63), m.ucmp(a, b), m.imaths(a, b)].join(","));
        }
        out.push(m.conv(I[i]), m.cond(I[i]));
    }
    for (i = 0; i < D.length; i++) {
        out.push(m.trunc(D[i]), Object.is(m.negzero(D[i]), -0), m.maths(D[i]));
        for (j = 0; j < D.length; j++)
            out.push(m.dbl(D[i], D[j]), m.fcmp(D[i], D[j]), m.flt(D[i], D[j]));
    }
    out.push(m.add("12", {valueOf: function () { return 30; }}), m.add(), m.trunc("1e3"), m.dbl());
    m.heapw(64);
    out.push(m.heapr(64), m.heapf(64), new Int8Array(buf, 64, 24).join("."));
    out.push(m.oob(-4), m.oob(0x10000), m.oob(0x10000 - 1), m.oob(0x7FFFFFF8), m.oobf(-8), m.oobf(0x10000), m.oobf(0xFFF8));
    out.push(new Int32Array(buf, 0xFFF0, 4).join("."));
    for (i = 0; i < 4; i++)
        out.push(m.globals(i));
    out.push(m.calls(10), asmLog.join(" "));
    out.push(m.loops(20), m.fib(15), m.deep(1000));
    try { m.deep(1e7); out.push("no throw"); } catch (e) { out.push(e instanceof RangeError); }
    throwAt = 2;
    try { out.push(m.calls(4)); } catch (e) { out.push(e instanceof TypeError, e.message); }
    // A heap detached by an import: the module's reads give 0 and NaN, its
    // writes do nothing.
    var buf2 = new ArrayBuffer(0x10000);
    var b2 = buf2;
    var m2 = AsmEdges(globalThis, {
        ffiInt: function () { return 0; }, ffiDouble: function () { return 0; },
        ffiVoid: function (i) { if (i == 1) b2.transfer(); }, start: 0, scale: 0
    }, buf2);
    m2.heapw(8);
    out.push(m2.heapr(8), m2.calls(3), m2.heapr(8), m2.heapf(8), m2.oob(8), buf2.byteLength);
    var n = NotAsm(globalThis, {}, new ArrayBuffer(0x10000));
    out.push(n.f(12345));
    out.push(asmNaNs());
    return out.join("\n");
}
