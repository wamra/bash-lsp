#!/bin/bash
# assumption: this script is run from its own directory
host_hook() { echo hook; }
source ./lib/needs.sh
run_needs
