#!/bin/bash
reader() {
  echo "$mode"
}
outer() {
  local mode=fast
  reader
}
mode=slow
outer
reader
