#!/bin/bash
fact() {
  local n=$1
  if [ "$n" -le 1 ]; then echo 1; return; fi
  echo $(( n * $(fact $((n - 1))) ))
}
fact 5
