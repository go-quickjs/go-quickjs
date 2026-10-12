package vm

import "testing"

// TestBigIntResultRooms pins sums, differences and products whose digits are
// kept in the BigInt's own allocation, sized 4, 8 or 16 words, or on their
// own past that, on both tiers: operands either side of each size's edge,
// negative ones, and microbench's sum of squares. The answers are Node's.
func TestBigIntResultRooms(t *testing.T) {
	defer SetTreeTier(true)
	const src = `function f() {
  var out = [], xs = [];
  for (var k of [63, 64, 65, 127, 128, 191, 255, 256, 257, 447, 511, 512, 513, 1023, 1024, 1025, 2047]) {
    var p = 1n << BigInt(k);
    xs.push(p - 1n, p, p + 12345n, -(p - 3n));
  }
  var h = 0n;
  for (var i = 0; i < xs.length; i++) for (var j = 0; j < xs.length; j += 3) {
    var a = xs[i], b = xs[j];
    h = (h * 31n + (a + b) + (a - b) * 7n + a * b) % (1n << 4096n);
    if (i % 5 == 0) out.push((a * b).toString(16).length, (a + b).toString(36).slice(-6));
  }
  var s = 0n, x = 1n << 123n;
  for (var i = 0; i < 1000; i++) { s += x * x; x += 1n; }
  out.push(h.toString(16).slice(0, 40), h.toString(16).length, s.toString(16), x);
  return out.join(",");
}
f()`
	const want = `32,64sgse,33,2,32,976ypk,32,fbz5z3,48,2vskxq,49,wr045a,48,2p7708,64,iarwn3,80,mo658e,81,gjdofy,80,69yblk,80,9hhvr3,128,ncopoe,129,h7w8vy,144,ob9ooo,144,9k4lxb,144,g1uzge,145,9x2iny,272,mau0jc,272,5j99mn,272,804av2,273,1vbu2m,528,agxxko,32,976p6n,33,32e8eb,33,c9l73t,33,iededc,48,5y6tbz,49,tolvr1,49,5rlfeh,64,ld651c,80,pqkdmn,81,dgzg1p,81,9ccjzt,81,cjw45c,128,qf2y2n,129,e5i0hp,144,rdnx2x,145,cmiubk,145,j497un,146,6uoa9p,272,pd88xl,273,8lni0w,273,b2ij9b,274,ysxlod,528,djc5yx,33,fbzfi0,34,976ypo,33,iedxf6,33,oj64op,49,c2zjnc,50,njt5fo,49,bwe5pu,65,rhyvcp,81,vvd3y0,82,7c6pqc,81,fh5ab6,81,ioougp,129,wjvoe0,130,80pa6c,145,xignea,145,irbkmx,145,p91y60,146,0pvjyc,273,vi0z8y,273,eqg8c9,273,h7b9ko,274,so4vd0,529,jo4waa,49,wr045a,48,2vskxm,49,tolm84,49,njteyl,65,2,64,zmsp2y,65,ztem2k,81,fezbpf,97,jsdkaq,96,jf69dm,97,3e5qnw,97,6lpatf,145,kgw4qq,144,k3ottm,161,lfh3r0,161,6oc0zn,161,d62eiq,160,csv3lm,289,jf1flo,289,2ngooz,289,54bpxe,288,4r4f0a,545,7l5cn0,64,iarwn2,65,c5zfuq,64,ld6ek8,64,rhyltr,80,f1s0se,81,fezbpe,80,ev6muw,96,ugrchr,112,yu5l32,113,4de8la,112,ifxrg8,112,lnhblr,160,zio5j2,161,51wt1a,176,0h94jc,176,lq41rz,176,s7ufb2,177,xr32ta,304,ygtge0,304,hp8phb,304,k63qpq,305,ppce7y,560,mmxdfc,80,mo658f,81,gjdog3,80,pqkn5l,81,vvcuf4,96,jf69dr,97,jsdkar,96,j8kvg9,112,yu5l34,128,37jtof,129,3,128,mtc01l,129,q0vk74,176,3w2e4f,177,0oikfx,192,4und4p,192,q3iadc,192,wl8nwf,193,tdou7x,320,2u7ozd,320,m2my2o,320,ojhzb3,321,lby5ml,576,r0bm0p,80,69yblk,81,055ut8,81,9cctiq,81,fh50s9,96,30yfqw,97,3e5qnw,97,2ud1te,112,ifxrg9,128,mtc01k,129,jls6d8,129,6f46eq,129,9mnqk9,176,nhukhk,177,h2qe2s,192,ogfjhu,193,9pagqh,193,g70u9k,194,9rwnus,320,mfzvci,321,5of4ft,321,85a5o8,322,1q5z9g,576,am3sdu,81,3cpeym,80,9hhvqy,82,0aax1g,82,u5iprx,97,6lpata,96,68hzwa,98,6saoqs,113,r6pz3x,129,mtbqim,128,q0vk6y,130,37jk5g,130,3,177,dv6txe,176,qpe4my,193,etrsxo,194,02mq6b,194,6kd3pe,193,jekeey,321,ctc4sc,322,w1rdvn,322,yimf42,321,bctptm,577,0zg1to,144,ob9f5q,145,i6gyde,144,rdnx2w,144,xig4cf,160,l29jb2,161,lfgu82,160,kvo5dk,176,0h8v0f,192,4un3lq,193,1n39xe,192,ogf9yw,192,rnyu4f,240,5j5o1q,241,0ykphe,256,6hqn20,256,rqlkan,256,y8bxtq,257,rqlkam,384,4haywo,384,npq7zz,384,q6l98e,385,jouvpa,640,snevy0,144,9k4lxb,145,3fc54z,145,cmj3uh,145,irbb40,160,6b4q2n,161,6oc0zn,161,64jc55,176,lq41s0,192,q3iadb,193,mvygoz,193,9pagqh,193,cwu0w0,240,qs0utb,241,m7fw8z,256,rqlttl,257,czgr28,257,jh74lb,258,6hqdj1,384,pq65o9,385,8ylerk,385,bfgfzz,386,yfzoxp,640,dwa2pl,144,g1v8zc,145,9x2s70,145,j49qwi,145,p91y61,160,csvd4o,161,d62o1o,161,cm9z76,177,s7uou1,192,wl8xfc,193,tdp3r0,193,g713si,193,jekny1,240,x9rhvc,241,sp6jb0,257,y8cgvm,257,jh7e49,257,pyxrnc,258,9j0,385,w7wsqa,385,fgc1tl,385,hx7320,386,ry91vo,641,ke0prm,273,g61a7y,272,matr0a,273,d3msas,273,6yul19,289,jf162m,288,j1tv5m,289,jlmk04,305,401ud9,321,zmnlry,320,2u7fga,321,g0vfes,321,ctbv99,369,yy51by,368,3ipzwa,385,xzk2bo,385,cqp531,385,68yrjy,384,w7w9oa,513,9j0,513,j8fimb,513,lpajuq,512,o65l2y,769,o646kc,272,804av2,273,1vbu2q,273,b2iss8,273,h7b01r,288,4r4f0e,289,54bpxe,289,4kj12w,304,k63qpr,320,ojhzb2,321,lby5mq,321,85a5o8,321,bctptr,368,p80jr2,369,knfl6q,385,q6lirc,385,bfgfzz,385,hx6tj2,386,ry9beq,513,o65um0,513,7el3pb,513,9vg4xq,514,2,768,cc9rnc,528,agxo1r,529,4c579f,528,djc5yx,529,jo4d8g,544,77xs73,545,7l5343,544,71ce9l,560,mmx3wg,576,r0bchr,577,nsritf,576,am3iux,577,dtn30g,624,rotwxr,625,n48ydf,640,snevy1,640,dw9t6o,640,ke06pr,641,uf2olf,768,qmz7sp,768,9vegw0,768,cc9i4f,769,2gtd6r,1024,et34u1,bd78d5f2a7596087c34875ef3cfec9ac5928da8d,1024,fa0000000000000000000000000079f2c0000000000000000000000013d6a2dc,10633823966279326983230456482242757608`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
	}
}
