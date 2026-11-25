#!/bin/bash
cat <(p=1; echo "$p")
echo "${p:-none}"
{ b=1; }
echo "$b"
if true; then c=1; fi
echo "$c"
