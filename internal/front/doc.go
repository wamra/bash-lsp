// Package front is the only package allowed to import the shell parser, a
// patched copy of mvdan.cc/sh/v3/syntax kept in internal/front/syntax
// (enforced by boundary_test.go at the repo root). It lowers the parser's
// syntax tree into the parser-independent tree package.
package front
