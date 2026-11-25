#!/bin/bash
before_fi() { echo before; }
if true; then
  echo inside
after_fi() { echo after; }
