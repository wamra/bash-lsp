#!/bin/bash
shopt -s expand_aliases
status_all() { echo all; }
alias gs=status_all
gs
alias aa=status_all; aa
alias zz=status_all
unalias zz
zz
