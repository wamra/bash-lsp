package front

import (
	"testing"
	"time"
)

// Found by FuzzBind in analysis: Parse did not return on this input.
func TestParseReturnsOnFuzzBindInput(t *testing.T) {
	src := []byte("${0=$((((\x11ke1\x05]@\x00\x00\x00\xad\xaeq\x16@\"\x80>\x00\x83|k(((($")
	done := make(chan struct{})
	go func() {
		Parse("x.sh", src, Bash)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Parse still running after 10 s")
	}
}
