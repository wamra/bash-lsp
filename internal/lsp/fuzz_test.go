package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"testing"
)

// FuzzFrameReader feeds arbitrary bytes to the connection reader. It must
// neither panic nor hang: every call consumes input or returns an error, so
// the loop ends with the input.
func FuzzFrameReader(f *testing.F) {
	frame := func(body string) []byte { return []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)) }
	f.Add(frame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	f.Add(append(frame(`{}`), frame(`{"method":"exit"}`)...))
	f.Add([]byte("Content-Length: 1099511627776\r\n\r\nshort"))
	f.Add([]byte("Content-Length: -1\r\n\r\n"))
	f.Add([]byte("Content-Length: 5\r\nContent-Type: x\r\n\r\nabc"))
	f.Add([]byte("Content-Length: 9223372036854775807\r\n\r\n"))
	f.Add([]byte("\r\n\r\ncontent-length:2\n\nab"))
	f.Add([]byte("Content-Length 5\r\n\r\nabcde"))
	f.Add(bytes.Repeat([]byte("x"), 70<<10))
	f.Fuzz(func(t *testing.T, data []byte) {
		c := newConn(bytes.NewReader(data), io.Discard)
		for i := 0; i <= len(data)+1; i++ { // each successful read consumes at least its header
			if _, err := c.read(); err != nil {
				return
			}
		}
		t.Fatalf("the reader returned %d frames from %d bytes", len(data)+2, len(data))
	})
}

// A header that announces a huge body followed by a short one fails with
// io.ErrUnexpectedEOF and does not allocate what it announced.
func TestFrameLengthIsNotPreallocated(t *testing.T) {
	for _, length := range []string{"1099511627776", "9223372036854775807", "4294967296"} {
		data := []byte("Content-Length: " + length + "\r\n\r\nshort body")
		c := newConn(bytes.NewReader(data), io.Discard)
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		_, err := c.read()
		runtime.ReadMemStats(&after)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("length %s: error %v, want io.ErrUnexpectedEOF", length, err)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Fatalf("length %s: TotalAlloc grew by %d bytes", length, grew)
		}
	}
}
