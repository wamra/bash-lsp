// Package front is the only package allowed to import the mvdan.cc/sh parser
// (enforced by boundary_test.go at the repo root). It lowers the mvdan syntax
// tree into the parser-independent tree package.
package front

// The blank import keeps mvdan.cc/sh/v3 pinned in go.mod until the lowering
// code lands.
import _ "mvdan.cc/sh/v3/syntax"
