#!/bin/bash
if [ -n "$FAST" ]; then
  impl() { echo fast; }
else
  impl() { echo slow; }
fi
impl
