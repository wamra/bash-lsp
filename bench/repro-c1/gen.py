import os, sys
base = "/private/tmp/wbrr/cases"
N = 900000
cases = {
 # unterminated quote on line 1 hides the rest from the lexer; resync re-parses line 2
 "resync_ternary": "echo '\n" + "echo $(( " + "a?"*N + "1 ))\n",
 "resync_not": "echo '\n" + "[[ " + "! "*N + "x ]]\n",
 "resync_if": "echo '\n" + "if x; then "*150000 + "\n",
 "control_ternary": "echo $(( " + "a?"*N + "1 ))\n",
}
for name, body in cases.items():
    d = os.path.join(base, name); os.makedirs(d, exist_ok=True)
    open(os.path.join(d, "x.sh"), "w").write(body)
    print(name, len(body))
