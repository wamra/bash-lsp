#!/bin/bash
one() { :; }
multi() {
  echo a
  echo b
}
if [ -n "$1" ]; then
  echo yes
else
  echo no
fi
for i in 1 2; do
  echo "$i"
done
case "$1" in
  a)
    echo a
    ;;
  *) echo other ;;
esac
