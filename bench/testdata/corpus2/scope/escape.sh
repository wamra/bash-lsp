#!/bin/bash
( inner=1 )
echo "${inner:-unset}"
v=$(w=5; echo "$w")
echo "${w:-unset}"
