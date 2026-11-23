import os
base = "/private/tmp/wbrr/cases"
N = 900000
cases = {
 # heredoc inside $(( $(...) )): the lexer treats << as a shift (arith>0), so it scans the body as code and the ' there opens a quote to EOF
 "heredoc_in_arith": "x=$(( $(cat <<EOF\n'\nEOF\n) ))\necho $(( " + "a?"*N + "1 ))\n",
 # small sanity variant: same prefix, shallow body, should parse
 "heredoc_in_arith_small": "x=$(( $(cat <<EOF\n'\nEOF\n) ))\necho $(( a?1:2 ))\nf() { :; }\n",
 "resync_small": "echo '\nf() { :; }\n",
}
for name, body in cases.items():
    d = os.path.join(base, name); os.makedirs(d, exist_ok=True)
    open(os.path.join(d, "x.sh"), "w").write(body)
