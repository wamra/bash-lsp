package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

// JSON-RPC error codes used by the server.
const (
	codeParseError       = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternalError    = -32603
	codeNotInitialized   = -32002
	codeRequestCancelled = -32800 // LSP 3.17 LSPErrorCodes.RequestCancelled
)

// message is a request, a notification, or a response to a request the
// server sent (no method; result or error).
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"` // kept raw: a response is read leniently
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// errBadFrame marks a header block with a missing or invalid Content-Length.
// The body, if any, was not consumed and its length is unknown, so the stream
// cannot be re-aligned: it is fatal for the session.
var errBadFrame = errors.New("bad frame")

// conn reads and writes Content-Length framed messages.
type conn struct {
	r *bufio.Reader

	wmu  sync.Mutex
	w    io.Writer
	werr error
}

func newConn(in io.Reader, out io.Writer) *conn {
	return &conn{r: bufio.NewReaderSize(in, 64<<10), w: out}
}

// scratchMax is the largest body read into the caller's reusable buffer; a
// longer body is read into a buffer that grows with the bytes received, so a
// bogus Content-Length costs nothing until the bytes arrive.
const scratchMax = 64 << 10

// read returns the next message body. The body size is not limited; memory
// grows only with the bytes actually received.
func (c *conn) read() ([]byte, error) { return c.readBuf(nil) }

// readBuf is read with a reusable buffer: when buf has room for the body (and
// the body is at most scratchMax bytes) the result is buf[:n], so the caller
// must not keep it past the next call.
func (c *conn) readBuf(buf []byte) ([]byte, error) {
	length := -1
	sawHeader := false
	for {
		line, err := c.line()
		if err != nil {
			if err == io.EOF && !sawHeader && len(line) == 0 {
				return nil, io.EOF
			}
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if !sawHeader {
				continue // stray blank line between messages
			}
			break
		}
		sawHeader = true
		name, val, ok := bytes.Cut(line, []byte(":"))
		if ok && bytes.EqualFold(bytes.TrimSpace(name), []byte("Content-Length")) {
			n, err := strconv.ParseInt(string(bytes.TrimSpace(val)), 10, 64)
			if err != nil || n < 0 {
				length = -2
			} else {
				length = int(n)
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("%w: missing or invalid Content-Length", errBadFrame)
	}
	if length <= scratchMax {
		var body []byte
		if cap(buf) >= length {
			body = buf[:length]
		} else {
			body = make([]byte, length)
		}
		if _, err := io.ReadFull(c.r, body); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return body, nil
	}
	var big bytes.Buffer
	if _, err := io.CopyN(&big, c.r, int64(length)); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return big.Bytes(), nil
}

// line reads up to and including the next newline. The result is valid until
// the next read; a line longer than the buffer is assembled in a copy.
func (c *conn) line() ([]byte, error) {
	frag, err := c.r.ReadSlice('\n')
	if err != bufio.ErrBufferFull {
		return frag, err
	}
	long := append([]byte(nil), frag...)
	for err == bufio.ErrBufferFull {
		frag, err = c.r.ReadSlice('\n')
		long = append(long, frag...)
	}
	return long, err
}

func (c *conn) send(v any) {
	body, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.werr != nil {
		return
	}
	frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))), body...)
	if _, err := c.w.Write(frame); err != nil {
		c.werr = err
	}
}

func (c *conn) writeErr() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.werr
}

func (c *conn) reply(id json.RawMessage, result any, rerr *rpcError) {
	resp := response{JSONRPC: "2.0", ID: id}
	if len(resp.ID) == 0 {
		resp.ID = json.RawMessage("null")
	}
	if rerr != nil {
		resp.Error = rerr
	} else {
		b, err := json.Marshal(result)
		if err != nil {
			resp.Error = &rpcError{codeInternalError, err.Error()}
		} else {
			resp.Result = b
		}
	}
	c.send(resp)
}

// request sends a request to the client; its response arrives as a message
// with no method.
func (c *conn) request(id, method string, params any) {
	c.send(request{JSONRPC: "2.0", ID: id, Method: method, Params: params})
}

func (c *conn) notify(method string, params any) {
	c.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}
