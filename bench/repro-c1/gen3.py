import os
base="/private/tmp/wbrr/cases"
cases={
 "scan_slow_blanks": "echo $(( " + "a="*990 + " "*2000000 + "1 ))\n",
 "scan_slow_plus": "echo $(( " + "a="*990 + "+"*2000000 + "1 ))\n",
 "scan_slow_ctrl": "echo $(( " + "a="*5 + " "*2000000 + "1 ))\n",
}
for n,b in cases.items():
    d=os.path.join(base,n); os.makedirs(d,exist_ok=True); open(os.path.join(d,"x.sh"),"w").write(b); print(n,len(b))
