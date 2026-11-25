#!/bin/bash
show() {
  echo "$target"
}
deploy() {
  local target=prod
  show
}
deploy
