#!/bin/bash
emit() { echo data; }
out=$(emit)
emit | cat
( emit )
if emit; then :; fi
