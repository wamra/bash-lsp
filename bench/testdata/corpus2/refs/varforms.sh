#!/bin/bash
name=a
name+=b
echo "$name" "${name}" "${#name}"
echo $((name_x + 1))
echo "${name:-default}"
