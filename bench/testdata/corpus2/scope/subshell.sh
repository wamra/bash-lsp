#!/bin/bash
a=outer
( a=sub; echo "$a" )
echo "$a"
