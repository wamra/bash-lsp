#!/bin/bash
v=1
f() {
  local v=2
  echo "$v"
}
g() {
  echo "$v"
}
f
g
echo "$v"
