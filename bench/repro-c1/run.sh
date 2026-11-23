#!/bin/bash
c=$1
s=$(date +%s)
perl -e 'alarm shift; exec @ARGV' 120 ./bash-lsp symbols cases/$c --json > out_$c.txt 2> err_$c.txt
rc=$?
e=$(date +%s)
echo "$c rc=$rc t=$((e-s))s out=$(head -c 160 out_$c.txt | tr '\n' ' ') err=$(grep -m1 -E 'fatal|stack exceeds|panic' err_$c.txt)"
