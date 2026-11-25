#!/bin/bash
for f in ./plug/*.sh; do
  . "$f"
done
plug_one
plug_two
