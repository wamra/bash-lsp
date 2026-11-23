#!/usr/bin/env bash
HERE="$(dirname "$0")"
# shellcheck source=lib/util.sh
source "$HERE/lib/util.sh"
build_release dyn
