#!/bin/bash
tmp() { echo t; }
tmp
unset -f tmp
tmp
