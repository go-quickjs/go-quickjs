;; GC arrays as a compiled GC language makes them: many short ones -- a
;; pair of f64s (array.new_fixed), a box of four i32s (array.new of a
;; constant length), a string of 1 to 16 i16s (array.new_default of a
;; length computed), a tuple of three references (array.new_fixed) -- and a
;; few long ones, the backing array of a growable list of the tuples,
;; doubled by a copy when full and walked and dropped at 4096 tuples.
(module
  (type $f64s (array (mut f64)))
  (type $i32s (array (mut i32)))
  (type $chars (array (mut i16)))
  (type $objs (array (mut (ref null eq))))

  (func $point (param $i i32) (result (ref $f64s))
    (array.new_fixed $f64s 2
      (f64.convert_i32_u (local.get $i))
      (f64.mul (f64.convert_i32_u (local.get $i)) (f64.const 0.5))))

  (func $str (param $i i32) (result (ref $chars))
    (local $s (ref null $chars)) (local $n i32) (local $k i32)
    (local.set $n (i32.add (i32.and (local.get $i) (i32.const 15)) (i32.const 1)))
    (local.set $s (array.new_default $chars (local.get $n)))
    (loop $l
      (array.set $chars (local.get $s) (local.get $k)
        (i32.add (i32.const 97)
          (i32.rem_u (i32.add (local.get $i) (local.get $k)) (i32.const 26))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (local.get $n))))
    (ref.as_non_null (local.get $s)))

  (func $hash (param $s (ref $chars)) (result i32)
    (local $h i32) (local $k i32)
    (loop $l
      (local.set $h
        (i32.add (i32.mul (local.get $h) (i32.const 31))
          (array.get_u $chars (local.get $s) (local.get $k))))
      (local.set $k (i32.add (local.get $k) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $k) (array.len (local.get $s)))))
    (local.get $h))

  (func (export "run") (param $n i32) (result i64)
    (local $list (ref null $objs)) (local $grown (ref null $objs))
    (local $size i32) (local $k i32) (local $i i32) (local $acc i64)
    (local $p (ref null $f64s)) (local $b (ref null $i32s)) (local $t (ref null $objs))
    (local.set $list (array.new_default $objs (i32.const 4)))
    (loop $l
      (local.set $p (call $point (local.get $i)))
      (local.set $b (array.new $i32s (local.get $i) (i32.const 4)))
      (array.set $i32s (local.get $b) (i32.const 3) (i32.const 7))
      (local.set $t
        (array.new_fixed $objs 3
          (local.get $p) (ref.i31 (local.get $i)) (call $str (local.get $i))))
      (local.set $acc
        (i64.add (i64.mul (local.get $acc) (i64.const 31))
          (i64.add
            (i64.trunc_f64_s
              (f64.add (array.get $f64s (local.get $p) (i32.const 0))
                       (array.get $f64s (local.get $p) (i32.const 1))))
            (i64.extend_i32_u
              (i32.add
                (call $hash (ref.cast (ref $chars) (array.get $objs (local.get $t) (i32.const 2))))
                (i32.add (array.get $i32s (local.get $b) (i32.const 0))
                         (array.get $i32s (local.get $b) (i32.const 3))))))))
      (if (i32.eq (local.get $size) (array.len (local.get $list)))
        (then
          (local.set $grown
            (array.new_default $objs (i32.shl (local.get $size) (i32.const 1))))
          (array.copy $objs $objs (local.get $grown) (i32.const 0)
            (local.get $list) (i32.const 0) (local.get $size))
          (local.set $list (local.get $grown))))
      (array.set $objs (local.get $list) (local.get $size) (local.get $t))
      (local.set $size (i32.add (local.get $size) (i32.const 1)))
      (if (i32.eq (local.get $size) (i32.const 4096))
        (then
          (local.set $k (i32.const 0))
          (loop $w
            (local.set $acc
              (i64.xor (local.get $acc)
                (i64.extend_i32_s
                  (i31.get_s
                    (ref.cast (ref i31)
                      (array.get $objs
                        (ref.cast (ref $objs) (array.get $objs (local.get $list) (local.get $k)))
                        (i32.const 1)))))))
            (local.set $k (i32.add (local.get $k) (i32.const 1)))
            (br_if $w (i32.lt_u (local.get $k) (local.get $size))))
          (local.set $list (array.new_default $objs (i32.const 4)))
          (local.set $size (i32.const 0))))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $acc)))
