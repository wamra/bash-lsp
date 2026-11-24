"""Write the adversarial inputs of the final review's Bind/Build hunt (review C1 and its siblings), each about
2 MiB, one file per shape. Usage: gen_bind_shapes.py OUTDIR [BYTES]
BYTES (default 2 MiB - 64) is the size budget of each shape; at the default the output is byte for byte what
m2a's final review probed. Pass half of it to time each shape at n and 2n. Counts that are not sizes (depths,
int_assign_100k) do not scale.
Shapes the parser refuses at its nesting limit (long `$((`, `${a[` runs, a 2 MiB `(( a+a+... ))`) are kept: their
row shows the input never reaches Bind. A 2 MiB single expression of names is reached through `let "..."`.
The m2b shapes stress edges, string bodies, variable calls, `unset -f`, aliases and many
definitions of one name; the m3 shapes (diag_*, last block) the native diagnostic rules."""
import os, sys
out = sys.argv[1]
M0 = 2 * 1024 * 1024 - 64
M = int(sys.argv[2]) if len(sys.argv) > 2 else M0
def rep(u, pre="", post=""):
    return pre + u * ((M - len(pre) - len(post)) // len(u)) + post
def deep(d, inner, pre="", post="\n"):
    return pre + "( " * d + inner + " )" * d + post
expr = "x=$(( a[" + "b+" * 4000 + "1] ))\n"
shapes = {
    "let_unclosed_brackets": rep("a[", 'let "', '"\n'),
    "int_unclosed_brackets": rep("a[", "declare -i n\nn=", "\n"),
    "int_balanced_brackets": "declare -i n\nn=" + "a[" * (699000 * M // M0) + "1" + "]" * (699000 * M // M0) + "\n",
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
# m2b: edges, strings, variable calls, unset -f, aliases, many definitions of one name.
def lines(unit, n0, pre=""):  # n0 lines at the default size, scaled with M
    return pre + unit * (n0 * M // M0)
fdef = "f() { :; }\n"
shapes["eval_strings_100k"] = lines('eval "f $x"\n', 100000, fdef)
shapes["eval_one_big_string"] = rep("f;", fdef + 'eval "', '"\n')
shapes["eval_nested_strings"] = rep("eval \"eval 'f'\";", fdef)
shapes["trap_shell_c_strings"] = rep("trap 'f' INT; bash -c 'f';", fdef)
for d in (1950, 3900):
    shapes[f"deep_cmd_calls_{d}"] = deep(d, '"$cmd";' * ((M - 4 * d - 30) // 7), fdef + "cmd=f\n")
    shapes[f"deep_func_cmd_calls_{d}"] = deep(d, 'cmd=f; "$cmd";' * ((M - 4 * d - 40) // 14), fdef + "g() { ", " }\n")
    shapes[f"deep_func_calls_{d}"] = deep(d, "f;" * ((M - 4 * d - 30) // 2), fdef + "g() { ", " }\n")
    shapes[f"deep_func_defs_{d}"] = deep(d, "h() { :; };" * ((M - 4 * d - 30) // 11), "g() { ", " }\n")
    shapes[f"deep_func_plain_assigns_{d}"] = deep(d, "y=1;" * ((M - 4 * d - 20) // 4), "f() { ", " }\n")
shapes["deep_func_assigns_1950"] = deep(1950, "((y=1));" * ((M - 4 * 1950 - 20) // 8), "f() { ", " }\n")
shapes["deep_cmd_call_one_3900"] = deep(3900, '"$cmd"', fdef + "cmd=f\n")
shapes["unset_f_100k"] = lines("unset -f f; f\n", 100000, fdef)
shapes["alias_chain_70k"] = "".join(f"alias a{i}=a{i + 1}\n" for i in range(70000 * M // M0)) + "a0\n"
shapes["func_defs_20k"] = lines(fdef, 20000) + "f\n"  # one call: one edge per definition
shapes["func_defs_2k_calls_2k"] = lines(fdef, 2000) + lines("f\n", 2000)  # 4 million edges: output is calls x definitions
shapes["source_var_decls"] = lines("x=1\n", 100000) + lines(". $f\n", 100000)
shapes["alias_fn_70k"] = fdef + "".join(f"alias a{i}=f\n" for i in range(70000 * M // M0)) + "a0\n"
shapes["func_defs_reads_20k"] = lines("f() { x=1; echo $y; }\n", 20000) + "f\n"
shapes["deep_func_sources_3900"] = deep(3900, ". $f;" * ((M - 4 * 3900 - 20) // 5), "g() { ", " }\n")
shapes["deep_aliases_3900"] = deep(3900, "alias a=f;" * ((M - 4 * 3900 - 20) // 10), fdef)
# dynamic scope (fix round 1): unresolved reads in f, calls of f from h or from many callers
q = M // 2
shapes["dyn_reads_calls"] = "f() { " + "echo $y;" * (q // 8) + " }\nh() { " + "f;" * (q // 2) + " }\n"
shapes["dyn_reads_calls_local"] = "f() { " + "echo $y;" * (q // 8) + " }\nh() { local y; " + "f;" * (q // 2) + " }\n"
shapes["dyn_reads_callers"] = "f() { " + "echo $y;" * (q // 8) + " }\n" + "".join(f"h{i}() {{ f; }}\n" for i in range(q // 17))
shapes["dyn_names_callers_local"] = ("f() { " + "".join(f"echo $v{i};" for i in range(q // 14)) + " }\n"
                                     + "".join(f"h{i}() {{ local w; f; }}\n" for i in range(q // 26)))
# fix round 2: the read name is a local of many callers
shapes["dyn_local_callers"] = "f() { echo $y; }\n" + "".join(f"h{i}() {{ local y; f; }}\n" for i in range(M // 27))
shapes["dyn_local_callers_3"] = ("f() { echo $y; }\ng() { echo $y; }\nk() { echo $y; }\n"
                                 + "".join(f"h{i}() {{ local y; f; g; k; }}\n" for i in range(M // 33)))
for d in (1000, 1990):
    shapes[f"nested_funcs_defs_{d}"] = "f() { " * d + "h() { :; };" * ((M - 8 * d) // 11) + " }" * d + "\n"
# m3 shapes: the native diagnostic rules (analysis/rules.go) that walk scopes, bindings, declarations or
# suppression directives, each with the input that would multiply a careless rule.
shapes["diag_unused_locals_dyn"] = rep('f() { local u=1; "$h"; }\n')
shapes["diag_reads_after_subshells"] = rep('( y=1 ); echo "$y"\n')
d = 3000
shapes["diag_nested_directives_other"] = rep("rm $f\n", "{\n# shellcheck disable=SC2034\n" * d, "}\n" * d)
shapes["diag_nested_directives_same"] = rep("rm $f\n", "{\n# shellcheck disable=SC2086\n" * d, "}\n" * d)
shapes["diag_directive_per_command"] = rep("# shellcheck disable=SC2086\nrm $f\n", "echo start\n")
shapes["diag_global_assigned_often"] = rep("x=1\n", "#!/bin/bash\necho hi\n")
shapes["diag_unused_dup_functions"] = rep("f() { :; }\n", "#!/bin/bash\necho hi\n")
shapes["diag_unquoted_args"] = rep(" $f", "rm", "\n")
shapes["diag_reads_in_strings"] = rep("eval 'echo $u';", "f() { local u=1; ", " }\nf\n")
parts, size = ["f() {"], 5
while size < M - 20:
    parts.append(f" local v{len(parts)}=1;")
    size += len(parts[-1])
shapes["diag_distinct_locals"] = "".join(parts) + " }\nf\n"
for k, v in shapes.items():
    open(os.path.join(out, k + ".sh"), "w").write(v)
print(len(shapes), "shapes")
