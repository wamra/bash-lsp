#!/bin/bash
leaf() { echo leaf; }
mid() { leaf; leaf; }
top() { mid; leaf; }
orphan_fn() { :; }
recur() { recur; }
mutual_a() { mutual_b; }
mutual_b() { mutual_a; }
top
