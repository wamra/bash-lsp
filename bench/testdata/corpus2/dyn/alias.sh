#!/bin/bash
shopt -s expand_aliases
status_all() { echo all; }
aa() { echo real_aa; }
zz() { echo real_zz; }
alias gs=status_all
gs
alias aa=status_all; aa
alias zz=status_all
unalias zz
zz
