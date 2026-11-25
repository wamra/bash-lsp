#!/bin/bash
printf '%s\n' a b | while read x; do echo "$x"; done
echo hi | read z
echo "${z:-none}"
n=0
seq 3 | while read i; do n=$((n + 1)); done
echo "$n"
