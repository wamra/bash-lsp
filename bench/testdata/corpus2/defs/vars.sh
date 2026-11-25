#!/bin/bash
LIMIT=10
count=0
bump() {
  count=$((count + 1))
  [ "$count" -gt "$LIMIT" ] && echo over
}
report() {
  echo "count=$count"
}
bump
report
