// RFC 1321 MD5 over byte strings (code units 0..255), for the JIT benchmarks.
var md5Constants = [
    0xd76aa478, 0xe8c7b756, 0x242070db, 0xc1bdceee,
    0xf57c0faf, 0x4787c62a, 0xa8304613, 0xfd469501,
    0x698098d8, 0x8b44f7af, 0xffff5bb1, 0x895cd7be,
    0x6b901122, 0xfd987193, 0xa679438e, 0x49b40821,
    0xf61e2562, 0xc040b340, 0x265e5a51, 0xe9b6c7aa,
    0xd62f105d, 0x02441453, 0xd8a1e681, 0xe7d3fbc8,
    0x21e1cde6, 0xc33707d6, 0xf4d50d87, 0x455a14ed,
    0xa9e3e905, 0xfcefa3f8, 0x676f02d9, 0x8d2a4c8a,
    0xfffa3942, 0x8771f681, 0x6d9d6122, 0xfde5380c,
    0xa4beea44, 0x4bdecfa9, 0xf6bb4b60, 0xbebfbc70,
    0x289b7ec6, 0xeaa127fa, 0xd4ef3085, 0x04881d05,
    0xd9d4d039, 0xe6db99e5, 0x1fa27cf8, 0xc4ac5665,
    0xf4292244, 0x432aff97, 0xab9423a7, 0xfc93a039,
    0x655b59c3, 0x8f0ccc92, 0xffeff47d, 0x85845dd1,
    0x6fa87e4f, 0xfe2ce6e0, 0xa3014314, 0x4e0811a1,
    0xf7537e82, 0xbd3af235, 0x2ad7d2bb, 0xeb86d391
];
var md5Shifts = [
    7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22, 7, 12, 17, 22,
    5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20, 5, 9, 14, 20,
    4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23, 4, 11, 16, 23,
    6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21, 6, 10, 15, 21
];

function md5blk(s) {
    var block = new Array(16);
    for (var i = 0; i < 64; i += 4) {
        block[i >> 2] = s.charCodeAt(i) | (s.charCodeAt(i + 1) << 8) |
            (s.charCodeAt(i + 2) << 16) | (s.charCodeAt(i + 3) << 24);
    }
    return block;
}

function md5cycle(state, block) {
    var a = state[0], b = state[1], c = state[2], d = state[3];
    for (var i = 0; i < 64; i++) {
        var f, g;
        if (i < 16) {
            f = (b & c) | (~b & d);
            g = i;
        } else if (i < 32) {
            f = (d & b) | (~d & c);
            g = (5 * i + 1) & 15;
        } else if (i < 48) {
            f = b ^ c ^ d;
            g = (3 * i + 5) & 15;
        } else {
            f = c ^ (b | ~d);
            g = (7 * i) & 15;
        }
        var sum = (a + f + md5Constants[i] + block[g]) | 0;
        var shift = md5Shifts[i];
        var next = (b + ((sum << shift) | (sum >>> (32 - shift)))) | 0;
        a = d;
        d = c;
        c = b;
        b = next;
    }
    state[0] = (state[0] + a) | 0;
    state[1] = (state[1] + b) | 0;
    state[2] = (state[2] + c) | 0;
    state[3] = (state[3] + d) | 0;
}

function md5(s) {
    var state = [0x67452301, 0xefcdab89, 0x98badcfe, 0x10325476];
    var offset = 0;
    for (; offset + 64 <= s.length; offset += 64) {
        md5cycle(state, md5blk(s.slice(offset, offset + 64)));
    }
    var tail = new Array(16).fill(0);
    var remaining = s.length - offset;
    for (var i = 0; i < remaining; i++) {
        tail[i >> 2] |= s.charCodeAt(offset + i) << ((i & 3) * 8);
    }
    tail[remaining >> 2] |= 0x80 << ((remaining & 3) * 8);
    if (remaining >= 56) {
        md5cycle(state, tail);
        tail = new Array(16).fill(0);
    }
    tail[14] = s.length << 3;
    tail[15] = Math.floor(s.length / 0x20000000);
    md5cycle(state, tail);

    var hex = "0123456789abcdef", result = "";
    for (var word = 0; word < 4; word++) {
        for (var byte = 0; byte < 4; byte++) {
            var value = (state[word] >>> (byte * 8)) & 255;
            result += hex[value >>> 4] + hex[value & 15];
        }
    }
    return result;
}
