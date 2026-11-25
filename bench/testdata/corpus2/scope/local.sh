#!/bin/bash
x=global
f() {
  local x=inner
  echo "$x"
}
g() {
  echo "$x"
}
f
g
