#!/bin/bash
ok_fn() { echo ok; }
ok_fn
if [[ -f "$1" ]
then
  echo file
fi
