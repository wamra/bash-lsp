#!/bin/bash
cd() { builtin cd "$@" && echo "now in $PWD"; }
cd /tmp
builtin cd /
command cd /
