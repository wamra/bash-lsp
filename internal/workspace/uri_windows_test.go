//go:build windows

package workspace

import (
	"strings"
	"testing"
)

func TestURIWindows(t *testing.T) {
	p, _ := PathFromURI("file:///C:/Users/a%20b/x.sh")
	if p != `C:\Users\a b\x.sh` {
		t.Fatal(p)
	}
	p2, _ := PathFromURI("file:///c%3A/Users/a%20b/x.sh")
	if !strings.EqualFold(p, p2) {
		t.Fatal(p2)
	}
	if URIFromPath(p) != "file:///C:/Users/a%20b/x.sh" {
		t.Fatal(URIFromPath(p))
	}
}
