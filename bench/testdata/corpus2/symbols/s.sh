#!/bin/bash
# Compute the sum of the arguments.
sum() {
  local total=0
  local n
  for n in "$@"; do total=$((total + n)); done
  echo "$total"
}

VERSION=1.2
readonly LIMIT=5

function helper { :; }

alias sm=sum
sum 1 2

