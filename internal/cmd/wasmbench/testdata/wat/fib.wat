;; Recursive calls: fib(n) by its definition, two calls a call.
(module
  (func $fib (param $n i32) (result i64)
    (if (result i64) (i32.lt_u (local.get $n) (i32.const 2))
      (then (i64.extend_i32_u (local.get $n)))
      (else
        (i64.add
          (call $fib (i32.sub (local.get $n) (i32.const 1)))
          (call $fib (i32.sub (local.get $n) (i32.const 2)))))))

  (func (export "run") (param $n i32) (result i64)
    (call $fib (local.get $n))))
