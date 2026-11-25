#!/bin/bash
dyn_call() { echo "$1"; }
eval "dyn_call arg"
for n in one two; do
  eval "gen_$n() { echo $n; }"
done
gen_one
