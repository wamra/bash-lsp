#!/bin/bash
declare -a arr=(one two "three four")
declare -A map=([k]=v)
for e in "${arr[@]}"; do
  case "$e" in
    one) echo 1 ;;
    *) echo other ;;
  esac
done
[[ -n ${map[k]} && $e == t* ]] && echo yes
diff <(echo a) <(echo b)
echo done fi esac then
