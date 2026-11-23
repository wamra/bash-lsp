package workspace

import (
	"path/filepath"
	"testing"
)

func TestURI(t *testing.T) {
	p, err := PathFromURI("file:///tmp/a%20b/x.sh")
	if err != nil || p != filepath.FromSlash("/tmp/a b/x.sh") {
		t.Fatal(p, err)
	}
	if got := URIFromPath(p); got != "file:///tmp/a%20b/x.sh" {
		t.Fatal(got)
	}
	if _, err := PathFromURI("untitled:Untitled-1"); err == nil {
		t.Fatal("non-file URI accepted")
	}
}

// The conversions are tested for both OS forms on any host.
func TestURIForms(t *testing.T) {
	p, err := pathFromURI("file:///C:/Users/a%20b/x.sh", true)
	if err != nil || p != `C:\Users\a b\x.sh` {
		t.Fatal(p, err)
	}
	p2, err := pathFromURI("file:///c%3A/Users/a%20b/x.sh", true)
	if err != nil || p2 != `C:\Users\a b\x.sh` {
		t.Fatal(p2, err)
	}
	if got := uriFromPath(p, true); got != "file:///C:/Users/a%20b/x.sh" {
		t.Fatal(got)
	}
	if got := uriFromPath(`c:\Users\x.sh`, true); got != "file:///C:/Users/x.sh" {
		t.Fatal(got)
	}
	unc, err := pathFromURI("file://srv/share/x.sh", true)
	if err != nil || unc != `\\srv\share\x.sh` {
		t.Fatal(unc, err)
	}
	if got := uriFromPath(unc, true); got != "file://srv/share/x.sh" {
		t.Fatal(got)
	}
	if _, err := pathFromURI("file://srv/share/x.sh", false); err == nil {
		t.Fatal("remote host accepted on POSIX")
	}
	lh, err := pathFromURI("file://localhost/tmp/x.sh", false)
	if err != nil || lh != "/tmp/x.sh" {
		t.Fatal(lh, err)
	}
	q, err := pathFromURI("FILE:///tmp/a%23b%25c.sh", false)
	if err != nil || q != "/tmp/a#b%c.sh" {
		t.Fatal(q, err)
	}
	if got := uriFromPath("/tmp/a#b%c é.sh", false); got != "file:///tmp/a%23b%25c%20%C3%A9.sh" {
		t.Fatal(got)
	}
	if back, _ := pathFromURI(uriFromPath("/tmp/a#b%c é.sh", false), false); back != "/tmp/a#b%c é.sh" {
		t.Fatal(back)
	}
}
