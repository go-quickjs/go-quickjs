// Every typed operation of asm.js over edge operands, for the JIT's
// differential test (TestAsmJITOps): run natively, by the typed
// interpreter and as ordinary code, each must give the same answers.
function AsmOps(stdlib, foreign, heap) {
    "use asm";
    var I8 = new stdlib.Int8Array(heap);
    var U8 = new stdlib.Uint8Array(heap);
    var I16 = new stdlib.Int16Array(heap);
    var U16 = new stdlib.Uint16Array(heap);
    var I32 = new stdlib.Int32Array(heap);
    var U32 = new stdlib.Uint32Array(heap);
    var F32 = new stdlib.Float32Array(heap);
    var F64 = new stdlib.Float64Array(heap);
    var imul = stdlib.Math.imul;
    var clz32 = stdlib.Math.clz32;
    var abs = stdlib.Math.abs;
    var min = stdlib.Math.min;
    var max = stdlib.Math.max;
    var sqrt = stdlib.Math.sqrt;
    var floor = stdlib.Math.floor;
    var ceil = stdlib.Math.ceil;
    var sin = stdlib.Math.sin;
    var exp = stdlib.Math.exp;
    var pow = stdlib.Math.pow;
    var atan2 = stdlib.Math.atan2;
    var fround = stdlib.Math.fround;
    var gi = 0;
    var gd = 0.0;

    function iop(k, a, b) {
        k = k | 0;
        a = a | 0;
        b = b | 0;
        switch (k | 0) {
        case 0: return (a + b) | 0;
        case 1: return (a - b) | 0;
        case 2: return imul(a, b) | 0;
        case 3: return ((a | 0) / (b | 0)) | 0;
        case 4: return ((a | 0) % (b | 0)) | 0;
        case 5: return ((a >>> 0) / (b >>> 0)) | 0;
        case 6: return ((a >>> 0) % (b >>> 0)) | 0;
        case 7: return (a & b) | 0;
        case 8: return (a | b) | 0;
        case 9: return (a ^ b) | 0;
        case 10: return (a << b) | 0;
        case 11: return (a >> b) | 0;
        case 12: return (a >>> b) | 0;
        case 13: return (-a) | 0;
        case 14: return (~a) | 0;
        case 15: return (!a) | 0;
        case 16: return clz32(a) | 0;
        case 17: return abs(a | 0) | 0;
        case 18: return min(a | 0, b | 0) | 0;
        case 19: return max(a | 0, b | 0) | 0;
        case 20: return ((a | 0) < (b | 0)) | 0;
        case 21: return ((a | 0) <= (b | 0)) | 0;
        case 22: return ((a | 0) > (b | 0)) | 0;
        case 23: return ((a | 0) >= (b | 0)) | 0;
        case 24: return ((a | 0) == (b | 0)) | 0;
        case 25: return ((a | 0) != (b | 0)) | 0;
        case 26: return ((a >>> 0) < (b >>> 0)) | 0;
        case 27: return ((a >>> 0) <= (b >>> 0)) | 0;
        case 28: return ((a >>> 0) > (b >>> 0)) | 0;
        case 29: return ((a >>> 0) >= (b >>> 0)) | 0;
        case 30: return (a * 7) | 0;
        case 31: return (a + 1000) | 0;
        }
        return 0;
    }

    function ibr(a, b) {
        a = a | 0;
        b = b | 0;
        var s = 0;
        if ((a | 0) < (b | 0)) s = s | 1;
        if ((a | 0) <= (b | 0)) s = s | 2;
        if ((a | 0) > (b | 0)) s = s | 4;
        if ((a | 0) >= (b | 0)) s = s | 8;
        if ((a | 0) == (b | 0)) s = s | 16;
        if ((a | 0) != (b | 0)) s = s | 32;
        if ((a >>> 0) < (b >>> 0)) s = s | 64;
        if ((a >>> 0) <= (b >>> 0)) s = s | 128;
        if ((a | 0) < 3) s = s | 256;
        if ((a | 0) > -3) s = s | 512;
        if (a) s = s | 1024;
        if (!a) s = s | 2048;
        return s | 0;
    }

    function dop(k, a, b) {
        k = k | 0;
        a = +a;
        b = +b;
        switch (k | 0) {
        case 0: return +(a + b);
        case 1: return +(a - b);
        case 2: return +(a * b);
        case 3: return +(a / b);
        case 4: return +(a % b);
        case 5: return +(-a);
        case 6: return +abs(a);
        case 7: return +sqrt(a);
        case 8: return +fround(a);
        case 9: return +floor(a);
        case 10: return +ceil(a);
        case 11: return +sin(a);
        case 12: return +exp(a);
        case 13: return +pow(a, b);
        case 14: return +atan2(a, b);
        case 15: return +min(a, b);
        case 16: return +max(a, b);
        case 17: return +(((a < b) | 0) >>> 0);
        case 18: return +(((a <= b) | 0) >>> 0);
        case 19: return +(((a == b) | 0) >>> 0);
        case 20: return +(((a != b) | 0) >>> 0);
        case 21: return +((~~a) | 0);
        case 22: return +((~~a) >>> 0);
        }
        return 0.0;
    }

    function dbr(a, b) {
        a = +a;
        b = +b;
        var s = 0;
        if (a < b) s = s | 1;
        if (a <= b) s = s | 2;
        if (a > b) s = s | 4;
        if (a >= b) s = s | 8;
        if (a == b) s = s | 16;
        if (a != b) s = s | 32;
        if (!(a < b)) s = s | 64;
        if (!(a <= b)) s = s | 128;
        return s | 0;
    }

    function store(k, p, v) {
        k = k | 0;
        p = p | 0;
        v = +v;
        switch (k | 0) {
        case 0: I8[p] = ~~v; break;
        case 1: U8[p] = ~~v; break;
        case 2: I16[p >> 1] = ~~v; break;
        case 3: U16[p >> 1] = ~~v; break;
        case 4: I32[p >> 2] = ~~v; break;
        case 5: U32[p >> 2] = ~~v; break;
        case 6: F32[p >> 2] = v; break;
        case 7: F64[p >> 3] = v; break;
        }
    }

    function load(k, p) {
        k = k | 0;
        p = p | 0;
        switch (k | 0) {
        case 0: return +(I8[p] | 0);
        case 1: return +(U8[p] | 0);
        case 2: return +(I16[p >> 1] | 0);
        case 3: return +(U16[p >> 1] | 0);
        case 4: return +(I32[p >> 2] | 0);
        case 5: return +(U32[p >> 2] >>> 0);
        case 6: return +F32[p >> 2];
        case 7: return +F64[p >> 3];
        }
        return 0.0;
    }

    function loadOff(p) {
        p = p | 0;
        return ((I32[(p + 8) >> 2] | 0) + (U8[(p + 3) | 0] | 0)) | 0;
    }

    function globals(a, d) {
        a = a | 0;
        d = +d;
        gi = (gi + a) | 0;
        gd = gd + d;
        return +(+(gi | 0) + gd);
    }

    function loop(n) {
        n = n | 0;
        var i = 0, s = 0, d = 0.0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
            s = (s + imul(i, i)) | 0;
            s = s ^ (s >>> 3);
            d = d + +(i | 0) * 0.5;
            I32[((i & 63) << 2) >> 2] = s;
            s = (s + (I32[(((i + 1) & 63) << 2) >> 2] | 0)) | 0;
        }
        return +(+(s | 0) + d);
    }

    return { iop: iop, ibr: ibr, dop: dop, dbr: dbr, store: store, load: load, loadOff: loadOff, globals: globals, loop: loop };
}

var asmOpsModule = AsmOps(globalThis, {}, new ArrayBuffer(65536));

function asmOps() {
    var m = asmOpsModule, out = [], i, j, k;
    var ints = [0, 1, -1, 2, 7, -7, 31, 32, 33, 255, 65535, -65536, 0x40000000, 0x7fffffff, -0x80000000, -0x7fffffff];
    var dbls = [0, -0, 1.5, -2.5, 0.5, NaN, Infinity, -Infinity, 1e300, 2147483648, 4294967296.5, -4294967297.25, 3.7, 1e-310];
    var fmt = function (x) { return Object.is(x, -0) ? "-0" : String(x); };
    for (k = 0; k < 32; k++) {
        var row = [];
        for (i = 0; i < ints.length; i++)
            for (j = 0; j < ints.length; j++)
                row.push(m.iop(k, ints[i], ints[j]));
        out.push("iop " + k + ": " + row.join(","));
    }
    var brs = [];
    for (i = 0; i < ints.length; i++)
        for (j = 0; j < ints.length; j++)
            brs.push(m.ibr(ints[i], ints[j]));
    out.push("ibr: " + brs.join(","));
    for (k = 0; k < 23; k++) {
        var drow = [];
        for (i = 0; i < dbls.length; i++)
            for (j = 0; j < dbls.length; j++)
                drow.push(fmt(m.dop(k, dbls[i], dbls[j])));
        out.push("dop " + k + ": " + drow.join(","));
    }
    var dbs = [];
    for (i = 0; i < dbls.length; i++)
        for (j = 0; j < dbls.length; j++)
            dbs.push(m.dbr(dbls[i], dbls[j]));
    out.push("dbr: " + dbs.join(","));
    var addrs = [0, 1, 2, 3, 4, 6, 7, 8, 65528, 65532, 65535, 65536, 65540, -1, -4, -8, 0x7ffffff8, -0x80000000];
    for (k = 0; k < 8; k++) {
        var lrow = [];
        for (i = 0; i < addrs.length; i++) {
            m.store(k, addrs[i], -1234.75 - i * 31.5);
            for (j = 0; j < 8; j++)
                lrow.push(fmt(m.load(j, addrs[i])));
        }
        out.push("heap " + k + ": " + lrow.join(","));
    }
    var orow = [];
    for (i = 0; i < addrs.length; i++)
        orow.push(m.loadOff(addrs[i]));
    out.push("loadOff: " + orow.join(","));
    out.push("globals: " + [m.globals(5, 0.25), m.globals(-0x7fffffff, 1e300), m.globals(0x7fffffff, -Infinity)].join(","));
    out.push("loop: " + m.loop(1000) + "," + m.loop(0) + "," + m.loop(70000));
    return out.join("\n");
}
