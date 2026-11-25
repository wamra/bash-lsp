#!/bin/bash
# plain function definitions in both syntaxes

greet() {
  echo "hello $1"
}

function farewell {
  echo "bye $1"
}

function shout() {
  greet "$1"
  farewell "$1"
}

greet world
shout world
farewell world
