// asm_ops.js's operations through calls, for the JIT's differential test
// of typed callees inlined (TestAsmJITOpsThroughCalls): each operation a
// small function of the module, called by one that switches on the
// operation as asm_ops.js's functions do -- module variables written in a
// callee, the heap stored to and loaded from in callees, a callee's
// callee, a loop calling a callee and a callee with a loop -- exported
// under asm_ops.js's names, so that asmCalls(), asm_ops.js's driver run
// over this module, gives asm_ops.want.
function AsmCalls(stdlib, foreign, heap) {
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

    function addk(a, k) {
        a = a | 0;
        k = k | 0;
        return (a + k) | 0;
    }

    function i0(a, b) {
        a = a | 0;
        b = b | 0;
        return (a + b) | 0;
    }

    function i1(a, b) {
        a = a | 0;
        b = b | 0;
        return (a - b) | 0;
    }

    function i2(a, b) {
        a = a | 0;
        b = b | 0;
        return imul(a, b) | 0;
    }

    function i3(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) / (b | 0)) | 0;
    }

    function i4(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) % (b | 0)) | 0;
    }

    function i5(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) / (b >>> 0)) | 0;
    }

    function i6(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) % (b >>> 0)) | 0;
    }

    function i7(a, b) {
        a = a | 0;
        b = b | 0;
        return (a & b) | 0;
    }

    function i8(a, b) {
        a = a | 0;
        b = b | 0;
        return (a | b) | 0;
    }

    function i9(a, b) {
        a = a | 0;
        b = b | 0;
        return (a ^ b) | 0;
    }

    function i10(a, b) {
        a = a | 0;
        b = b | 0;
        return (a << b) | 0;
    }

    function i11(a, b) {
        a = a | 0;
        b = b | 0;
        return (a >> b) | 0;
    }

    function i12(a, b) {
        a = a | 0;
        b = b | 0;
        return (a >>> b) | 0;
    }

    function i13(a, b) {
        a = a | 0;
        b = b | 0;
        return (-a) | 0;
    }

    function i14(a, b) {
        a = a | 0;
        b = b | 0;
        return (~a) | 0;
    }

    function i15(a, b) {
        a = a | 0;
        b = b | 0;
        return (!a) | 0;
    }

    function i16(a, b) {
        a = a | 0;
        b = b | 0;
        return clz32(a) | 0;
    }

    function i17(a, b) {
        a = a | 0;
        b = b | 0;
        return abs(a | 0) | 0;
    }

    function i18(a, b) {
        a = a | 0;
        b = b | 0;
        return min(a | 0, b | 0) | 0;
    }

    function i19(a, b) {
        a = a | 0;
        b = b | 0;
        return max(a | 0, b | 0) | 0;
    }

    function i20(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) < (b | 0)) | 0;
    }

    function i21(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) <= (b | 0)) | 0;
    }

    function i22(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) > (b | 0)) | 0;
    }

    function i23(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) >= (b | 0)) | 0;
    }

    function i24(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) == (b | 0)) | 0;
    }

    function i25(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a | 0) != (b | 0)) | 0;
    }

    function i26(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) < (b >>> 0)) | 0;
    }

    function i27(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) <= (b >>> 0)) | 0;
    }

    function i28(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) > (b >>> 0)) | 0;
    }

    function i29(a, b) {
        a = a | 0;
        b = b | 0;
        return ((a >>> 0) >= (b >>> 0)) | 0;
    }

    function i30(a, b) {
        a = a | 0;
        b = b | 0;
        return (a * 7) | 0;
    }

    function i31(a, b) {
        a = a | 0;
        b = b | 0;
        return addk(a, 1000) | 0;
    }

    function iop(k, a, b) {
        k = k | 0;
        a = a | 0;
        b = b | 0;
        switch (k | 0) {
        case 0: return i0(a, b) | 0;
        case 1: return i1(a, b) | 0;
        case 2: return i2(a, b) | 0;
        case 3: return i3(a, b) | 0;
        case 4: return i4(a, b) | 0;
        case 5: return i5(a, b) | 0;
        case 6: return i6(a, b) | 0;
        case 7: return i7(a, b) | 0;
        case 8: return i8(a, b) | 0;
        case 9: return i9(a, b) | 0;
        case 10: return i10(a, b) | 0;
        case 11: return i11(a, b) | 0;
        case 12: return i12(a, b) | 0;
        case 13: return i13(a, b) | 0;
        case 14: return i14(a, b) | 0;
        case 15: return i15(a, b) | 0;
        case 16: return i16(a, b) | 0;
        case 17: return i17(a, b) | 0;
        case 18: return i18(a, b) | 0;
        case 19: return i19(a, b) | 0;
        case 20: return i20(a, b) | 0;
        case 21: return i21(a, b) | 0;
        case 22: return i22(a, b) | 0;
        case 23: return i23(a, b) | 0;
        case 24: return i24(a, b) | 0;
        case 25: return i25(a, b) | 0;
        case 26: return i26(a, b) | 0;
        case 27: return i27(a, b) | 0;
        case 28: return i28(a, b) | 0;
        case 29: return i29(a, b) | 0;
        case 30: return i30(a, b) | 0;
        case 31: return i31(a, b) | 0;
        }
        return 0;
    }

    function ibrLow(a, b) {
        a = a | 0;
        b = b | 0;
        var s = 0;
        if ((a | 0) < (b | 0)) s = s | 1;
        if ((a | 0) <= (b | 0)) s = s | 2;
        if ((a | 0) > (b | 0)) s = s | 4;
        if ((a | 0) >= (b | 0)) s = s | 8;
        if ((a | 0) == (b | 0)) s = s | 16;
        if ((a | 0) != (b | 0)) s = s | 32;
        return s | 0;
    }

    function ibrHigh(a, b) {
        a = a | 0;
        b = b | 0;
        var s = 0;
        if ((a >>> 0) < (b >>> 0)) s = s | 64;
        if ((a >>> 0) <= (b >>> 0)) s = s | 128;
        if ((a | 0) < 3) s = s | 256;
        if ((a | 0) > -3) s = s | 512;
        if (a) s = s | 1024;
        if (!a) s = s | 2048;
        return s | 0;
    }

    function ibr(a, b) {
        a = a | 0;
        b = b | 0;
        return (ibrLow(a, b) | 0) | (ibrHigh(a, b) | 0);
    }

    function d0(a, b) {
        a = +a;
        b = +b;
        return +(a + b);
    }

    function d1(a, b) {
        a = +a;
        b = +b;
        return +(a - b);
    }

    function d2(a, b) {
        a = +a;
        b = +b;
        return +(a * b);
    }

    function d3(a, b) {
        a = +a;
        b = +b;
        return +(a / b);
    }

    function d4(a, b) {
        a = +a;
        b = +b;
        return +(a % b);
    }

    function d5(a, b) {
        a = +a;
        b = +b;
        return +(-a);
    }

    function d6(a, b) {
        a = +a;
        b = +b;
        return +abs(a);
    }

    function d7(a, b) {
        a = +a;
        b = +b;
        return +sqrt(a);
    }

    function d8(a, b) {
        a = +a;
        b = +b;
        return +fround(a);
    }

    function d9(a, b) {
        a = +a;
        b = +b;
        return +floor(a);
    }

    function d10(a, b) {
        a = +a;
        b = +b;
        return +ceil(a);
    }

    function d11(a, b) {
        a = +a;
        b = +b;
        return +sin(a);
    }

    function d12(a, b) {
        a = +a;
        b = +b;
        return +exp(a);
    }

    function d13(a, b) {
        a = +a;
        b = +b;
        return +pow(a, b);
    }

    function d14(a, b) {
        a = +a;
        b = +b;
        return +atan2(a, b);
    }

    function d15(a, b) {
        a = +a;
        b = +b;
        return +min(a, b);
    }

    function d16(a, b) {
        a = +a;
        b = +b;
        return +max(a, b);
    }

    function d17(a, b) {
        a = +a;
        b = +b;
        return +(((a < b) | 0) >>> 0);
    }

    function d18(a, b) {
        a = +a;
        b = +b;
        return +(((a <= b) | 0) >>> 0);
    }

    function d19(a, b) {
        a = +a;
        b = +b;
        return +(((a == b) | 0) >>> 0);
    }

    function d20(a, b) {
        a = +a;
        b = +b;
        return +(((a != b) | 0) >>> 0);
    }

    function d21(a, b) {
        a = +a;
        b = +b;
        return +((~~a) | 0);
    }

    function d22(a, b) {
        a = +a;
        b = +b;
        return +((~~a) >>> 0);
    }

    function dop(k, a, b) {
        k = k | 0;
        a = +a;
        b = +b;
        switch (k | 0) {
        case 0: return +d0(a, b);
        case 1: return +d1(a, b);
        case 2: return +d2(a, b);
        case 3: return +d3(a, b);
        case 4: return +d4(a, b);
        case 5: return +d5(a, b);
        case 6: return +d6(a, b);
        case 7: return +d7(a, b);
        case 8: return +d8(a, b);
        case 9: return +d9(a, b);
        case 10: return +d10(a, b);
        case 11: return +d11(a, b);
        case 12: return +d12(a, b);
        case 13: return +d13(a, b);
        case 14: return +d14(a, b);
        case 15: return +d15(a, b);
        case 16: return +d16(a, b);
        case 17: return +d17(a, b);
        case 18: return +d18(a, b);
        case 19: return +d19(a, b);
        case 20: return +d20(a, b);
        case 21: return +d21(a, b);
        case 22: return +d22(a, b);
        }
        return 0.0;
    }

    function dbrLow(a, b) {
        a = +a;
        b = +b;
        var s = 0;
        if (a < b) s = s | 1;
        if (a <= b) s = s | 2;
        if (a > b) s = s | 4;
        if (a >= b) s = s | 8;
        return s | 0;
    }

    function dbrHigh(a, b) {
        a = +a;
        b = +b;
        var s = 0;
        if (a == b) s = s | 16;
        if (a != b) s = s | 32;
        if (!(a < b)) s = s | 64;
        if (!(a <= b)) s = s | 128;
        return s | 0;
    }

    function dbr(a, b) {
        a = +a;
        b = +b;
        return (dbrLow(a, b) | 0) | (dbrHigh(a, b) | 0);
    }

    function st0(p, v) {
        p = p | 0;
        v = +v;
        I8[p] = ~~v;
    }

    function st1(p, v) {
        p = p | 0;
        v = +v;
        U8[p] = ~~v;
    }

    function st2(p, v) {
        p = p | 0;
        v = +v;
        I16[p >> 1] = ~~v;
    }

    function st3(p, v) {
        p = p | 0;
        v = +v;
        U16[p >> 1] = ~~v;
    }

    function st4(p, v) {
        p = p | 0;
        v = +v;
        I32[p >> 2] = ~~v;
    }

    function st5(p, v) {
        p = p | 0;
        v = +v;
        U32[p >> 2] = ~~v;
    }

    function st6(p, v) {
        p = p | 0;
        v = +v;
        F32[p >> 2] = v;
    }

    function st7(p, v) {
        p = p | 0;
        v = +v;
        F64[p >> 3] = v;
    }

    function store(k, p, v) {
        k = k | 0;
        p = p | 0;
        v = +v;
        switch (k | 0) {
        case 0: st0(p, v); break;
        case 1: st1(p, v); break;
        case 2: st2(p, v); break;
        case 3: st3(p, v); break;
        case 4: st4(p, v); break;
        case 5: st5(p, v); break;
        case 6: st6(p, v); break;
        case 7: st7(p, v); break;
        }
    }

    function ld0(p) {
        p = p | 0;
        return +(I8[p] | 0);
    }

    function ld1(p) {
        p = p | 0;
        return +(U8[p] | 0);
    }

    function ld2(p) {
        p = p | 0;
        return +(I16[p >> 1] | 0);
    }

    function ld3(p) {
        p = p | 0;
        return +(U16[p >> 1] | 0);
    }

    function ld4(p) {
        p = p | 0;
        return +(I32[p >> 2] | 0);
    }

    function ld5(p) {
        p = p | 0;
        return +(U32[p >> 2] >>> 0);
    }

    function ld6(p) {
        p = p | 0;
        return +F32[p >> 2];
    }

    function ld7(p) {
        p = p | 0;
        return +F64[p >> 3];
    }

    function load(k, p) {
        k = k | 0;
        p = p | 0;
        switch (k | 0) {
        case 0: return +ld0(p);
        case 1: return +ld1(p);
        case 2: return +ld2(p);
        case 3: return +ld3(p);
        case 4: return +ld4(p);
        case 5: return +ld5(p);
        case 6: return +ld6(p);
        case 7: return +ld7(p);
        }
        return 0.0;
    }

    function loadOffAt(p) {
        p = p | 0;
        return ((I32[(p + 8) >> 2] | 0) + (U8[(p + 3) | 0] | 0)) | 0;
    }

    function loadOff(p) {
        p = p | 0;
        return loadOffAt(p) | 0;
    }

    function addGlobals(a, d) {
        a = a | 0;
        d = +d;
        gi = (gi + a) | 0;
        gd = gd + d;
    }

    function globals(a, d) {
        a = a | 0;
        d = +d;
        addGlobals(a, d);
        return +(+(gi | 0) + gd);
    }

    function loopStep(s, i) {
        s = s | 0;
        i = i | 0;
        s = (s + imul(i, i)) | 0;
        s = s ^ (s >>> 3);
        I32[((i & 63) << 2) >> 2] = s;
        return (s + (I32[(((i + 1) & 63) << 2) >> 2] | 0)) | 0;
    }

    function loopRun(n) {
        n = n | 0;
        var i = 0, s = 0, d = 0.0;
        for (i = 0; (i | 0) < (n | 0); i = (i + 1) | 0) {
            s = loopStep(s, i) | 0;
            d = d + +(i | 0) * 0.5;
        }
        return +(+(s | 0) + d);
    }

    function loop(n) {
        n = n | 0;
        return +loopRun(n);
    }

    return { iop: iop, ibr: ibr, dop: dop, dbr: dbr, store: store, load: load, loadOff: loadOff, globals: globals, loop: loop };
}

// asm_ops.js's driver over this module, which it reads from asmOpsModule.
function asmCalls() {
    asmOpsModule = AsmCalls(globalThis, {}, new ArrayBuffer(65536));
    return asmOps();
}
