//go:build race

package lsp

// raceEnabled is true when the tests run under the race detector, which
// slows this code several times over: wall-clock bounds are skipped then.
const raceEnabled = true
