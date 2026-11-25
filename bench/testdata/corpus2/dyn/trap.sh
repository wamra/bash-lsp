#!/bin/bash
cleanup() { rm -f /tmp/x; }
on_int() { echo interrupted; }
trap 'cleanup' EXIT
trap on_int INT
