#!/bin/bash
# run from this directory
source ./lib/a.sh
. ./lib/b.sh
source ./lib/deep/c.sh extra args
lib_a
lib_b
lib_c
