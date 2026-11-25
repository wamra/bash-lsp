#!/bin/bash
outer() {
  inner() { echo inner; }
  inner
}
outer
inner
