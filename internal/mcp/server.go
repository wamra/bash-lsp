// Package mcp is a small Model Context Protocol server over standard input and
// output: newline-delimited JSON-RPC 2.0, the tools of tools.go, read-only.
// It answers from the same query encoders as the command line
// (internal/query), refreshes the workspace before every tool call, keeps
// every path inside the root, caps what it reads and writes, and redacts
// credential-looking text from answers and logs. It starts nothing from the
// workspace: the one program it may run (code-index, for search_code) is
// chosen by internal/extool from user settings and PATH.
//
// Protocol facts are those of the go-sdk v1.8.0 module; docs/mcp.md has the
// line references.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/wamra/bash-lsp/analysis"
	"github.com/wamra/bash-lsp/internal/config"
	"github.com/wamra/bash-lsp/internal/extool"
	"github.com/wamra/bash-lsp/internal/query"
	"github.com/wamra/bash-lsp/internal/redact"
	"github.com/wamra/bash-lsp/internal/workspace"
)

const (
	// MaxFrame is the longest request line read; a longer one is skipped
	// without being held in memory and answered with an error.
	MaxFrame = 1 << 20
	// MaxResponse is the longest reply line written.
	MaxResponse = 1 << 20
	// MaxArgBytes is the most a tool's arguments may take.
	MaxArgBytes = 64 << 10

	maxNesting  = 32 // deepest JSON nesting a frame may have
	maxIDBytes  = 256
	maxMsgBytes = 300
	callTimeout = 60 * time.Second
)

// protocolVersions are the versions offered, newest first.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// negotiate is the client's version when it is offered, else the newest.
func negotiate(client string) string {
	if slices.Contains(protocolVersions, client) {
		return client
	}
	return protocolVersions[0]
}

// Tool is one callable tool. Call receives the raw arguments (an object, at
// most MaxArgBytes and nested at most 32 deep) and a context that carries the
// Session of the call (SessionFrom): the snapshot is already refreshed. A
// returned error becomes a result with isError set; the value is encoded as
// the JSON document of the result.
type Tool struct {
	Name        string                                                       `json:"name"`
	Description string                                                       `json:"description"`
	InputSchema map[string]any                                               `json:"inputSchema"`
	Call        func(ctx context.Context, args json.RawMessage) (any, error) `json:"-"`
}

var registry struct {
	mu    sync.RWMutex
	tools []Tool
}

// Register adds t to the tools the server offers, or replaces the tool of
// the same name. tools.go registers the ten tools of the server; another
// package can add its own the same way.
func Register(t Tool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for i := range registry.tools {
		if registry.tools[i].Name == t.Name {
			registry.tools[i] = t
			return
		}
	}
	registry.tools = append(registry.tools, t)
}

func lookupTool(name string) (Tool, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	for _, t := range registry.tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func listTools() []Tool {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return slices.Clone(registry.tools)
}

// Session is what a tool call may use: the root, the settings and the
// snapshot taken after the refresh of this call.
type Session struct {
	root string
	cfg  config.Settings
	ws   *workspace.Workspace
	prof *extool.Profile
	snap *analysis.Snapshot
}

type sessionKey struct{}

// SessionFrom returns the Session of the call ctx belongs to, nil outside a
// tool call.
func SessionFrom(ctx context.Context) *Session {
	s, _ := ctx.Value(sessionKey{}).(*Session)
	return s
}

// Root is the workspace root, resolved; paths in answers are relative to it.
func (s *Session) Root() string { return s.root }

// Snapshot is the snapshot of this call, taken after Workspace.Refresh.
func (s *Session) Snapshot() *analysis.Snapshot { return s.snap }

// Settings are the effective settings the server was started with.
func (s *Session) Settings() config.Settings { return s.cfg }

// Workspace is the workspace the server answers from.
func (s *Session) Workspace() *workspace.Workspace { return s.ws }

// Profile is the PATH profile external programs are looked up in.
func (s *Session) Profile() *extool.Profile { return s.prof }

type server struct {
	sess    Session
	version string
	log     *log.Logger
}

func newServer(root string, cfg config.Settings, version string, logw io.Writer) (*server, error) {
	ws, prof, err := query.OpenWith(root, cfg)
	if err != nil {
		return nil, err
	}
	return &server{
		sess:    Session{root: ws.Root(), cfg: cfg, ws: ws, prof: prof},
		version: version,
		log:     log.New(logw, "mcp: ", log.LstdFlags),
	}, nil
}

// Serve opens root and answers requests read from in, writing replies to out
// and log lines to logw, until in ends (a nil error) or out fails. Nothing but
// JSON-RPC messages, one per line, is written to out. logw passes through
// redact.Writer.
func Serve(in io.Reader, out io.Writer, logw io.Writer, root string, cfg config.Settings, version string) error {
	if logw == nil {
		logw = io.Discard
	}
	rw := redact.Writer(logw)
	s, err := newServer(root, cfg, version, rw)
	if err != nil {
		return err
	}
	s.log.Printf("serving %s", s.sess.root)
	err = s.run(in, out)
	if f, ok := rw.(interface{ Flush() error }); ok {
		f.Flush()
	}
	return err
}

// run reads frames from in until it ends and writes the replies to out.
func (s *server) run(in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		frame, tooBig, err := readFrame(r, MaxFrame)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var reply []byte
		if tooBig {
			s.log.Printf("a frame over %d bytes was skipped", MaxFrame)
			reply = errorReply(nil, codeInvalidRequest, "request is larger than 1 MiB")
		} else {
			reply = s.handle(frame)
		}
		if reply == nil {
			continue
		}
		if _, err := out.Write(append(reply, '\n')); err != nil {
			return err
		}
	}
}

// readFrame reads one line of at most max bytes. A longer line is read to its
// end without being kept, and tooBig is true. The last line may lack its
// newline. At the end of input with nothing read it returns the error.
func readFrame(r *bufio.Reader, max int) (frame []byte, tooBig bool, err error) {
	var buf []byte
	for {
		chunk, e := r.ReadSlice('\n')
		if !tooBig {
			if len(buf)+len(chunk) > max+1 { // +1: the newline
				tooBig, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case e == nil:
			return bytes.TrimRight(buf, "\r\n"), tooBig, nil
		case errors.Is(e, bufio.ErrBufferFull):
			continue
		default:
			if len(buf) == 0 && !tooBig {
				return nil, false, e
			}
			return bytes.TrimRight(buf, "\r\n"), tooBig, nil
		}
	}
}

// JSON-RPC error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

func errorReply(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	msg = safeClip(msg, maxMsgBytes)
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}{code, msg}})
	return b
}

func resultReply(id json.RawMessage, result any) []byte {
	b, err := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
	if err != nil {
		return errorReply(id, codeInternal, "cannot encode the result")
	}
	return b
}

// safeClip redacts s, then cuts it to n bytes at a rune boundary: a secret is
// never cut into a piece the patterns no longer match.
func safeClip(s string, n int) string { return clipUTF8(redact.String(s), n) }

func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// rawID is the id member as sent: set tells it was there at all (null counts).
type rawID struct {
	set bool
	raw json.RawMessage
}

func (r *rawID) UnmarshalJSON(b []byte) error {
	r.set, r.raw = true, append(json.RawMessage(nil), b...)
	return nil
}

// valid reports whether the id may be echoed: a string or a number of
// bounded length. null, booleans, objects and arrays are not ids.
func (r rawID) valid() bool {
	b := bytes.TrimSpace(r.raw)
	if len(b) == 0 || len(b) > maxIDBytes {
		return false
	}
	return b[0] == '"' || b[0] == '-' || b[0] >= '0' && b[0] <= '9'
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      rawID           `json:"id"`
	Method  *string         `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// deeper reports whether b nests arrays and objects more than limit deep
// (brackets inside strings do not count).
func deeper(b []byte, limit int) bool {
	depth, inStr := 0, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case inStr:
			if c == '\\' {
				i++
			} else if c == '"' {
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '[' || c == '{':
			if depth++; depth > limit {
				return true
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return false
}

// handle answers one frame: the reply line, or nil when none is due.
func (s *server) handle(frame []byte) (reply []byte) {
	var id json.RawMessage
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("internal error: %v", r)
			reply = errorReply(id, codeInternal, "internal error")
		}
	}()
	frame = bytes.TrimSpace(frame)
	switch {
	case len(frame) == 0:
		return nil
	case frame[0] == '[':
		return errorReply(nil, codeInvalidRequest, "batch requests are not supported")
	case deeper(frame, maxNesting):
		return errorReply(nil, codeInvalidRequest, "message is nested too deeply")
	case frame[0] != '{':
		if json.Valid(frame) {
			return errorReply(nil, codeInvalidRequest, "a request is a JSON object")
		}
		return errorReply(nil, codeParse, "parse error")
	}
	var req request
	if err := json.Unmarshal(frame, &req); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return errorReply(nil, codeParse, "parse error")
		}
		return errorReply(nil, codeInvalidRequest, "invalid request")
	}
	if req.Method == nil {
		return nil // a response from the client, or nothing we know
	}
	method := *req.Method
	if !req.ID.set { // a notification: never answered
		return nil // notifications/initialized and notifications/cancelled need no action: requests run one at a time
	}
	if !req.ID.valid() {
		return errorReply(nil, codeInvalidRequest, "id must be a string or a number")
	}
	id = bytes.TrimSpace(req.ID.raw)
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return errorReply(id, codeInvalidParams, "invalid initialize parameters")
			}
		}
		return resultReply(id, map[string]any{
			"protocolVersion": negotiate(p.ProtocolVersion),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "bash-lsp", "version": s.version},
			"instructions":    "Read-only queries over the shell scripts of one workspace: symbols, references, call graph, unresolved calls, diagnostics and search. Paths are relative to the workspace root. Every answer carries the snapshot generation, and \"truncated\": true when it was cut.",
		})
	case "ping":
		return resultReply(id, map[string]any{})
	case "tools/list":
		tools := listTools()
		out := make([]map[string]any, len(tools))
		for i, t := range tools {
			out[i] = map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema, "annotations": map[string]any{"readOnlyHint": true, "openWorldHint": false}}
		}
		return resultReply(id, map[string]any{"tools": out})
	case "tools/call":
		return s.callTool(id, req.Params)
	}
	if len(method) >= len("notifications/") && method[:len("notifications/")] == "notifications/" {
		return errorReply(id, codeInvalidRequest, "a notification takes no id")
	}
	return errorReply(id, codeMethodNotFound, "method not found: "+method)
}

// callTool runs a tools/call request.
func (s *server) callTool(id json.RawMessage, params json.RawMessage) []byte {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return errorReply(id, codeInvalidParams, "tools/call wants a name and arguments")
	}
	t, ok := lookupTool(p.Name)
	if !ok {
		return errorReply(id, codeInvalidParams, "unknown tool: "+p.Name)
	}
	args := bytes.TrimSpace(p.Arguments)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	}
	if args[0] != '{' {
		return errorReply(id, codeInvalidParams, "arguments must be an object")
	}
	if len(args) > MaxArgBytes {
		return errorReply(id, codeInvalidParams, fmt.Sprintf("arguments are larger than %d bytes", MaxArgBytes))
	}
	start := time.Now()
	s.sess.ws.Refresh()
	sess := s.sess
	sess.snap = s.sess.ws.Snapshot()
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), sessionKey{}, &sess), callTimeout)
	defer cancel()
	v, err := s.safeCall(ctx, t, args)
	if err != nil {
		s.log.Printf("tools/call %s failed: %v", safeClip(p.Name, 64), err)
		return textReply(id, safeClip(err.Error(), 4096), true)
	}
	reply, err := render(id, v)
	if err != nil {
		s.log.Printf("tools/call %s failed: %v", safeClip(p.Name, 64), err)
		return textReply(id, err.Error(), true)
	}
	s.log.Printf("tools/call %s ok in %v", safeClip(p.Name, 64), time.Since(start).Round(time.Millisecond))
	return reply
}

// safeCall runs the tool; a panic is an error of the call, not of the server.
func (s *server) safeCall(ctx context.Context, t Tool, args json.RawMessage) (v any, err error) {
	defer func() {
		if r := recover(); r != nil {
			v, err = nil, fmt.Errorf("internal error in %s", t.Name)
			s.log.Printf("panic in %s: %v", t.Name, r)
		}
	}()
	return t.Call(ctx, args)
}

// textReply is a tool result of one text content.
func textReply(id json.RawMessage, text string, isError bool) []byte {
	type content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	res := struct {
		Content []content `json:"content"`
		IsError bool      `json:"isError,omitempty"`
	}{[]content{{"text", text}}, isError}
	return resultReply(id, res)
}

// cuttable is an answer whose list can be shortened to fit the reply cap.
type cuttable interface {
	Len() int
	Cut(k int) any
}

// encode is the text of an answer: the CLI's encoding, credentials redacted.
func encode(v any) (string, error) {
	b, err := query.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(redact.JSON(b)), nil
}

// render builds the reply line of a successful call. The whole line, not
// the document, is held to MaxResponse: when it is over, the list is cut
// (never the bytes) to the most records that fit, and the answer says
// "truncated": true.
func render(id json.RawMessage, v any) ([]byte, error) {
	text, err := encode(v)
	if err != nil {
		return nil, fmt.Errorf("cannot encode the answer: %v", err)
	}
	reply := textReply(id, text, false)
	if len(reply) <= MaxResponse {
		return reply, nil
	}
	c, ok := v.(cuttable)
	if !ok || c.Len() == 0 {
		return nil, fmt.Errorf("the answer is larger than %d bytes and has no list to cut", MaxResponse)
	}
	try := func(k int) ([]byte, error) {
		t, err := encode(c.Cut(k))
		if err != nil {
			return nil, err
		}
		return textReply(id, t, false), nil
	}
	lo, hi := 0, c.Len() // the most records that fit is in [lo, hi)
	var best []byte
	for lo < hi {
		mid := (lo + hi) / 2
		r, err := try(mid)
		if err != nil {
			return nil, err
		}
		if len(r) <= MaxResponse {
			best, lo = r, mid+1
		} else {
			hi = mid
		}
	}
	if best == nil {
		r, err := try(0)
		if err != nil || len(r) > MaxResponse {
			return nil, fmt.Errorf("the answer is larger than %d bytes even without records", MaxResponse)
		}
		best = r
	}
	return best, nil
}
