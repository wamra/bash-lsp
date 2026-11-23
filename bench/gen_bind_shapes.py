"""Write the adversarial inputs of the final review's Bind/Build hunt (review C1 and its siblings), each about
2 MiB, one file per shape. Usage: gen_bind_shapes.py OUTDIR
Shapes the parser refuses at its nesting limit (long `$((`, `${a[` runs, a 2 MiB `(( a+a+... ))`) are kept: their
row shows the input never reaches Bind. A 2 MiB single expression of names is reached through `let "..."`."""
import os, sys
out = sys.argv[1]
M = 2 * 1024 * 1024 - 64
def rep(u, pre="", post=""):
    return pre + u * ((M - len(pre) - len(post)) // len(u)) + post
def deep(d, inner, pre="", post="\n"):
    return pre + "( " * d + inner + " )" * d + post
expr = "x=$(( a[" + "b+" * 4000 + "1] ))\n"
shapes = {
    "let_unclosed_brackets": rep("a[", 'let "', '"\n'),
    "int_unclosed_brackets": rep("a[", "declare -i n\nn=", "\n"),
    "int_balanced_brackets": "declare -i n\nn=" + "a[" * 699000 + "1" + "]" * 699000 + "\n",
    "let_parens": rep("(", 'let "', 'a"\n'),
    "let_ternary": rep("a?b:", 'let "', 'c"\n'),
    "let_commas": rep("a,", 'let "', 'a"\n'),
    "let_names": rep("a ", 'let "', '"\n'),
    "let_plus_names": rep("ab+", 'let "', 'c"\n'),
    "let_singlequote_names": rep("a ", "let '", "'\n"),
    "let_dollar_arith": rep("$((", 'let "', '"\n'),
    "let_param_sub": rep("${a[", 'let "', '"\n'),
    "let_blanks_incr": 'let "a' + " " * (M - 12) + '++"\n',
    "let_incr_blanks": 'let "++' + " " * (M - 12) + 'a"\n',
    "int_blanks_subscript": "declare -i n\nn=a[1]" + " " * (M - 40) + "\n",
    "arith_cmd_names": rep("a+", "(( ", "a ))\n"),
    "dollar_arith_names": rep("a+", "echo $(( ", "a ))\n"),
    "arith_cmd_commas": rep("a=1,", "(( ", "a ))\n"),
    "arith_paren_digits_1000": "echo $(( " + "(" * 1000 + "1" * (M - 2020) + ")" * 1000 + " ))\n",
    "arith_nested_dollar_digits_1000": "echo " + "$(( " * 1000 + "1" * (M - 8020) + " ))" * 1000 + "\n",
    "param_sub_many": rep("${a[i]} ", "echo ", "\n"),
    "assign_subscripts_many": rep("x[a+b]=1\n"),
    "array_literal_keys": rep("[i+1]=x ", "a=(", ")\n"),
    "int_assign_100k": "declare -i n\n" + "n=n+1\n" * 100000,
    "int_assign_2mib": rep("n=n+1\n", "declare -i n\n"),
    "arith_incr_lines": rep("((n++))\n"),
    "subshell_arith_writes": rep("( ((y=1)) )\n", "y=0\n"),
    "func_arith_writes": rep("f() { ((g++)); }\n"),
    "cstyle_quotes": rep('for ((i=0;i<"(";i++)); do :; done\n'),
    "dashA_many": rep("-a ", "echo ", "\n"),
}
for d in (300, 1000):
    shapes[f"param_nested_digits_{d}"] = "echo " + "${a[" * d + "1" * (M - 9 * d) + "]}" * d + "\n"
    shapes[f"param_nested_names_{d}"] = "echo " + "${a[" * d + "b" * (M - 9 * d) + "]}" * d + "\n"
for d in (1000, 1950, 3900):
    sfx = "" if d == 1000 else f"_{d}"
    shapes["deep_subshells_writes" + sfx] = deep(d, "((y=1));" * ((M - 4 * d - 10) // 8), "y=0\n")
    shapes["deep_subshells_exprs" + sfx] = deep(d, expr * ((M - 4 * d) // len(expr)))
for d in (1950, 3900):
    shapes[f"deep_func_subshell_reads_{d}"] = deep(d, "echo $b;" * ((M - 4 * d - 20) // 8), "f() { ", " }\n")
d = 3900
shapes["deep_source_vars_3900"] = deep(d, "source $f;" * ((M - 4 * d) // 10))
shapes["deep_int_assigns_3900"] = deep(d, "x=1;" * ((M - 4 * d - 20) // 4), "declare -i q\n")
shapes["deep_func_assigns_3900"] = deep(d, "((y=1));" * ((M - 4 * d - 20) // 8), "f() { ", " }\n")
for k, v in shapes.items():
    open(os.path.join(out, k + ".sh"), "w").write(v)
print(len(shapes), "shapes")
