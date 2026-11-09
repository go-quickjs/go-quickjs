;; GC structs and arrays: the binary-trees benchmark -- complete trees of
;; structs made and walked, depth 4 to n by twos, with a long-lived tree of
;; depth n -- then arrays of i32 made, filled and summed.
(module
  (type $node (struct (field $l (ref null $node)) (field $r (ref null $node))))
  (type $arr (array (mut i32)))

  (func $make (param $d i32) (result (ref $node))
    (if (result (ref $node)) (i32.eqz (local.get $d))
      (then (struct.new $node (ref.null $node) (ref.null $node)))
      (else
        (struct.new $node
          (call $make (i32.sub (local.get $d) (i32.const 1)))
          (call $make (i32.sub (local.get $d) (i32.const 1)))))))

  (func $check (param $t (ref null $node)) (result i32)
    (if (result i32) (ref.is_null (struct.get $node $l (local.get $t)))
      (then (i32.const 1))
      (else
        (i32.add (i32.const 1)
          (i32.add
            (call $check (struct.get $node $l (local.get $t)))
            (call $check (struct.get $node $r (local.get $t))))))))

  (func $arrays (param $n i32) (param $seed i32) (result i32)
    (local $a (ref null $arr)) (local $i i32) (local $s i32)
    (local.set $a (array.new $arr (local.get $seed) (local.get $n)))
    (loop $fill
      (array.set $arr (local.get $a) (local.get $i)
        (i32.add (array.get $arr (local.get $a) (local.get $i))
                 (i32.mul (local.get $i) (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $fill (i32.lt_u (local.get $i) (array.len (local.get $a)))))
    (local.set $i (i32.const 0))
    (loop $sum
      (local.set $s (i32.xor (i32.rotl (local.get $s) (i32.const 1))
                             (array.get $arr (local.get $a) (local.get $i))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $sum (i32.lt_u (local.get $i) (array.len (local.get $a)))))
    (local.get $s))

  (func (export "run") (param $max i32) (result i64)
    (local $long (ref null $node)) (local $d i32) (local $iters i32) (local $i i32)
    (local $acc i64)
    (local.set $long (call $make (local.get $max)))
    (local.set $d (i32.const 4))
    (block $done
      (loop $dl
        (br_if $done (i32.gt_u (local.get $d) (local.get $max)))
        (local.set $iters
          (i32.shl (i32.const 1)
            (i32.add (i32.sub (local.get $max) (local.get $d)) (i32.const 4))))
        (local.set $i (i32.const 0))
        (loop $il
          (local.set $acc
            (i64.add (local.get $acc)
              (i64.extend_i32_u (call $check (call $make (local.get $d))))))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (br_if $il (i32.lt_u (local.get $i) (local.get $iters))))
        (local.set $d (i32.add (local.get $d) (i32.const 2)))
        (br $dl)))
    (local.set $acc
      (i64.add (local.get $acc) (i64.extend_i32_u (call $check (local.get $long)))))
    (local.set $i (i32.const 0))
    (loop $al
      (local.set $acc
        (i64.add (i64.shl (local.get $acc) (i64.const 1))
          (i64.extend_i32_u (call $arrays (i32.const 10000) (local.get $i)))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $al (i32.lt_u (local.get $i) (i32.const 100))))
    (local.get $acc)))
