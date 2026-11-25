#!/bin/bash
# assumption: this script is run from its own directory
for f in ./plug/*.sh; do
  . "$f"
done
plug_one
plug_two
