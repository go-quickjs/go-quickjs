;; i64 arithmetic: splitmix64's mixing -- adds, shifts, xors and multiplies
;; -- with a remainder and a rotate, the kernel called 100 times.
(module
  (func $kernel (param $n i32) (param $seed i64) (result i64)
    (local $i i32) (local $s i64) (local $z i64) (local $acc i64)
    (local.set $s (local.get $seed))
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $s (i64.add (local.get $s) (i64.const 0x9E3779B97F4A7C15)))
        (local.set $z
          (i64.mul
            (i64.xor (local.get $s) (i64.shr_u (local.get $s) (i64.const 30)))
            (i64.const 0xBF58476D1CE4E5B9)))
        (local.set $z
          (i64.mul
            (i64.xor (local.get $z) (i64.shr_u (local.get $z) (i64.const 27)))
            (i64.const 0x94D049BB133111EB)))
        (local.set $z (i64.xor (local.get $z) (i64.shr_u (local.get $z) (i64.const 31))))
        (local.set $acc (i64.add (local.get $acc) (i64.rem_u (local.get $z) (i64.const 1000003))))
        (local.set $acc (i64.xor (local.get $acc) (i64.rotl (local.get $z) (i64.const 17))))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.get $acc))

  (func (export "run") (param $n i32) (result i64)
    (local $k i32) (local $acc i64)
    (loop $l
      (local.set $acc
        (i64.xor (local.get $acc)
          (call $kernel (local.get $n) (i64.extend_i32_u (local.get $k)))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (i32.const 100))))
    (local.get $acc)))
