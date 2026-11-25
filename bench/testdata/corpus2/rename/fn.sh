#!/bin/bash
compute() { echo 1; }
compute
x=$(compute)
if compute; then echo y; fi
