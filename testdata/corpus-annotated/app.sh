#!/usr/bin/env bash
# shellcheck source=lib/hooks.sh
source ./lib/hooks.sh
on_start() { echo starting; }
for f in plugins/*.sh; do
  # shellcheck source=plugins/a.sh
  . "$f"
done
run_hooks
plugin_a
