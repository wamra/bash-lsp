package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// JSON-RPC error codes used by the server.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeNotInitialized = -32002
)

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
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

// read returns the next message body. The body size is not limited; memory
// grows only with the bytes actually received.
func (c *conn) read() ([]byte, error) {
	length := -1
	sawHeader := false
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			if err == io.EOF && !sawHeader && line == "" {
				return nil, io.EOF
			}
			if err == io.EOF {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if !sawHeader {
				continue // stray blank line between messages
			}
			break
		}
		sawHeader = true
		name, val, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
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
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, c.r, int64(length)); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return buf.Bytes(), nil
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

func (c *conn) notify(method string, params any) {
	c.send(notification{JSONRPC: "2.0", Method: method, Params: params})
}
