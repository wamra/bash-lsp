#!/bin/bash
f() {
  local a=1
  ( a=5 )
  echo "$a"
}
f
