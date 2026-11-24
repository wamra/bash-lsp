"""Write one shape whose call-graph output grows with a product of two counts (docs/m1-deferred.md,
"M3 limits"). Usage:
  gen_product_shapes.py defs_calls OUT D C   D definitions of f, then C calls of f: C x D
                                             candidate/conditional-definition edges
  gen_product_shapes.py unset OUT N          N lines `f() { :; }; unset -f f; f`: N x N
                                             candidate/unset-in-view edges
  gen_product_shapes.py cmd OUT N            N functions f0..f(N-1), N assignments cmd=f0..cmd=f(N-1), N lines
                                             `"$cmd" x`: N x N candidate/variable-constant edges
  gen_product_shapes.py dynscope OUT R C     f() reading $y R times, then C functions `h<i>() { local y; f; }`:
                                             R x C candidate/dynamic-scope bindings
The output is written to OUT; its size in bytes is printed."""
import sys
kind, out = sys.argv[1], sys.argv[2]
if kind == "defs_calls":
    d, c = int(sys.argv[3]), int(sys.argv[4])
    text = "f(){ :;}\n" * d + "f\n" * c
elif kind == "unset":
    n = int(sys.argv[3])
    text = "f() { :; }; unset -f f; f\n" * n
elif kind == "cmd":
    n = int(sys.argv[3])
    text = "".join("f%d() { :; }\n" % i for i in range(n)) + "".join("cmd=f%d\n" % i for i in range(n)) + '"$cmd" x\n' * n
elif kind == "dynscope":
    r, c = int(sys.argv[3]), int(sys.argv[4])
    text = "f() { " + "echo $y;" * r + " }\n" + "".join("h%d() { local y; f; }\n" % i for i in range(c))
else:
    sys.exit("unknown shape " + kind)
open(out, "w").write(text)
print(len(text.encode()))
