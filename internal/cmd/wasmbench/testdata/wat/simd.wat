;; SIMD: a dot product of two arrays of 8,192 i32s, four lanes at a time,
;; its accumulator mixed with a shift, an xor and a shuffle, the kernel
;; called n times.
(module
  (memory 1)

  (func $init
    (local $i i32)
    (loop $l
      (i32.store (i32.shl (local.get $i) (i32.const 2))
        (i32.and (i32.add (i32.mul (local.get $i) (i32.const 7)) (i32.const 3)) (i32.const 0xFFFF)))
      (i32.store offset=32768 (i32.shl (local.get $i) (i32.const 2))
        (i32.and (i32.add (i32.mul (local.get $i) (i32.const 13)) (i32.const 1)) (i32.const 0xFF)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 8192)))))

  (func $dot (param $seed i32) (result i32)
    (local $i i32) (local $acc v128)
    (local.set $acc (i32x4.splat (local.get $seed)))
    (loop $l
      (local.set $acc
        (i32x4.add (local.get $acc)
          (i32x4.mul (v128.load (local.get $i)) (v128.load offset=32768 (local.get $i)))))
      (local.set $acc
        (v128.xor (local.get $acc) (i32x4.shr_u (local.get $acc) (i32.const 7))))
      (local.set $acc
        (i8x16.shuffle 4 5 6 7 8 9 10 11 12 13 14 15 0 1 2 3
          (local.get $acc) (local.get $acc)))
      (local.set $i (i32.add (local.get $i) (i32.const 16)))
      (br_if $l (i32.lt_u (local.get $i) (i32.const 32768))))
    (i32.add
      (i32.add (i32x4.extract_lane 0 (local.get $acc)) (i32x4.extract_lane 1 (local.get $acc)))
      (i32.add (i32x4.extract_lane 2 (local.get $acc)) (i32x4.extract_lane 3 (local.get $acc)))))

  (func (export "run") (param $n i32) (result i64)
    (local $k i32) (local $acc i64)
    (call $init)
    (loop $l
      (local.set $acc
        (i64.add (local.get $acc) (i64.extend_i32_u (call $dot (local.get $k)))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (local.get $n))))
    (local.get $acc)))
