;; br_table switches: each iteration jumps to one of eight cases by the top
;; bits of a hash, which a branch predictor cannot learn, the kernel called
;; 100 times.
(module
  (func $kernel (param $n i32) (param $seed i32) (result i32)
    (local $i i32) (local $acc i32) (local $h i32)
    (local.set $acc (local.get $seed))
    (loop $l
      (local.set $h
        (i32.mul (i32.xor (local.get $i) (local.get $acc)) (i32.const 0x9E3779B1)))
      (block $end
        (block $c7
          (block $c6
            (block $c5
              (block $c4
                (block $c3
                  (block $c2
                    (block $c1
                      (block $c0
                        (br_table $c0 $c1 $c2 $c3 $c4 $c5 $c6 $c7
                          (i32.shr_u (local.get $h) (i32.const 29))))
                      (local.set $acc (i32.add (local.get $acc) (i32.const 1)))
                      (br $end))
                    (local.set $acc (i32.xor (local.get $acc) (local.get $h)))
                    (br $end))
                  (local.set $acc (i32.sub (local.get $acc) (local.get $i)))
                  (br $end))
                (local.set $acc (i32.rotl (local.get $acc) (i32.const 3)))
                (br $end))
              (local.set $acc (i32.mul (local.get $acc) (i32.const 5)))
              (br $end))
            (local.set $acc (i32.add (local.get $acc) (i32.shr_u (local.get $h) (i32.const 16))))
            (br $end))
          (local.set $acc (i32.xor (local.get $acc) (i32.const 0x5bd1e995)))
          (br $end))
        (local.set $acc (i32.sub (local.get $acc) (i32.const 0x1234))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc))

  (func (export "run") (param $n i32) (result i64)
    (local $k i32) (local $acc i64)
    (loop $l
      (local.set $acc
        (i64.add (local.get $acc)
          (i64.extend_i32_u (call $kernel (local.get $n) (local.get $k)))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (i32.const 100))))
    (local.get $acc)))
