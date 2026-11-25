#!/bin/bash
# assumption: this script is run from its own directory
cyc_a() { echo a; }
source ./cyc_b.sh
cyc_b
