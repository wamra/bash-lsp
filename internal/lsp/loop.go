package lsp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"slices"
	"sync"
	"time"
)

// ---- the message loop ----
//
// Frames travel through three kinds of goroutine:
//
//   - the reader (readLoop) reads and parses frames, handles `$/cancelRequest`
//     and `exit` itself, and queues everything else on inbound;
//   - the message loop (serve) takes items in order and runs every handler, so
//     handlers still never run concurrently with each other;
//   - the folder worker (folderWorker) opens workspace folders, the one slow
//     part of a folder change, and posts the outcome back to the loop.

// inboundCap is the capacity of the inbound queue. When it is full the reader
// stops reading and the client's writes back up.
const inboundCap = 256

// inbound is one item of the queue: a parsed frame, a parse failure, a closure
// posted by a goroutine, or the end of input.
type inbound struct {
	msg message
	bad *rpcError
	fn  func()
	err error
}

// Test seams, nil in production. openFoldersHook runs before the first
// build's reconcile, folderJobHook before each folder job, requestHook before
// each handler.
var (
	hookMu                         sync.RWMutex
	openFoldersHook, folderJobHook func()
	requestHook                    func(method string)
)

func hooks() (open, job func(), req func(string)) {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return openFoldersHook, folderJobHook, requestHook
}

// liveReq is a request that was read and not yet answered.
type liveReq struct {
	cancelled bool          // guarded by server.liveMu
	done      chan struct{} // closed when the request is cancelled
}

// idKey is the id's JSON text: 1 and "1" are different ids.
func idKey(raw json.RawMessage) string { return string(raw) }

// post runs fn on the message loop. It may be called from any goroutine
// except the loop itself (it would wait on its own queue).
func (s *server) post(fn func()) {
	select {
	case s.posted <- fn:
	case <-s.done:
	}
}

// stageItem hands an item to the reader's second stage, waiting while it is
// full, and gives up when Serve has returned.
func (s *server) stageItem(it inbound) {
	select {
	case s.stage <- it:
	case <-s.done:
	}
}

// forward moves items from the stage to the bounded queue. Together the two
// hold up to 2*inboundCap frames, so an `exit` or a cancel that follows a
// full queue is still read; beyond that the reader stops and the client's
// writes back up.
func (s *server) forward() {
	for {
		select {
		case it := <-s.stage:
			select {
			case s.inbound <- it:
			case <-s.done:
				return
			}
		case <-s.done:
			return
		}
	}
}

// wait waits for the first workspace build. It returns false when the build
// is not finished and the current request was cancelled or `exit` arrived;
// the caller then gives up: a request answers codeRequestCancelled, a
// notification does nothing.
func (s *server) wait() bool {
	s.startWorkspace()
	select {
	case <-s.ready:
		return true
	default:
	}
	var cancel <-chan struct{} // nil for a notification: never ready
	if s.cur != nil {
		cancel = s.cur.done
	}
	select {
	case <-s.ready:
		return true
	case <-s.quit:
		return false
	case <-cancel:
		return false
	}
}

func cancelledError() *rpcError {
	return &rpcError{codeRequestCancelled, "request cancelled"}
}

// ---- reader ----

func (s *server) readLoop() {
	enqueue := s.stageItem
	var scratch []byte
	for {
		body, err := s.c.readBuf(scratch)
		if err != nil {
			enqueue(inbound{err: err})
			return
		}
		if cap(body) <= scratchMax && cap(body) > cap(scratch) {
			scratch = body[:0]
		}
		if id, ok := cancelFrameID(body); ok {
			s.cancelRequest(id)
			continue
		}
		// Unmarshal validates the syntax itself and reports a failure of it as a
		// *json.SyntaxError, so a separate json.Valid pass over a large body (a
		// didOpen carries the whole document) is not needed to tell the two apart.
		var m message
		if err := json.Unmarshal(body, &m); err != nil {
			var syn *json.SyntaxError
			if errors.As(err, &syn) {
				enqueue(inbound{bad: &rpcError{codeParseError, "parse error: invalid JSON"}})
			} else {
				enqueue(inbound{bad: &rpcError{codeInvalidRequest, "invalid request: " + err.Error()}})
			}
			continue
		}
		switch {
		case m.Method == "$/cancelRequest":
			var p struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(m.Params, &p) == nil {
				s.cancelRequest(p.ID)
			}
			continue
		case m.Method == "exit":
			// Closing quit frees a handler stuck in wait(); queuing the
			// message makes the loop stop once it has taken what came before.
			s.quitOnce.Do(func() { close(s.quit) })
			enqueue(inbound{msg: m})
			return
		case m.Method != "" && len(m.ID) > 0:
			s.liveMu.Lock()
			s.live[idKey(m.ID)] = &liveReq{done: make(chan struct{})}
			s.liveMu.Unlock()
		}
		enqueue(inbound{msg: m})
	}
}

// cancelRequest marks the live request with id as cancelled; an id that is
// not live (never sent, or already answered) is ignored.
func (s *server) cancelRequest(id []byte) {
	if len(id) == 0 {
		return
	}
	s.liveMu.Lock()
	lr := s.live[string(id)]
	hit := lr != nil && !lr.cancelled
	if hit {
		lr.cancelled = true
		close(lr.done)
	}
	s.liveMu.Unlock()
	if hit {
		select { // wake a loop that is waiting for input with requests held
		case s.wake <- struct{}{}:
		default:
		}
	}
}

func (s *server) liveOf(key string) *liveReq {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	return s.live[key]
}

// isCancelled reports whether the live request with id key was cancelled.
func (s *server) isCancelled(key string) bool {
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	lr := s.live[key]
	return lr != nil && lr.cancelled
}

// finish takes a request out of the live set: it has been answered.
func (s *server) finish(key string) {
	s.liveMu.Lock()
	delete(s.live, key)
	s.liveMu.Unlock()
}

// answer replies to a request and ends its life in the live set.
func (s *server) answer(id json.RawMessage, result any, rerr *rpcError) {
	s.finish(idKey(id)) // before the reply: a client may reuse the id the moment it reads it
	s.c.reply(id, result, rerr)
}

var cancelMethod = []byte("$/cancelRequest")

// cancelFrameID recognises a `$/cancelRequest` frame without decoding it into
// a message, so that a flood of them costs no allocation. ok is false for
// anything else (including a frame it cannot judge: escapes in a key or in the
// method), which then takes the general path; id is the raw params.id, nil
// when the frame has none.
func cancelFrameID(body []byte) (id []byte, ok bool) {
	if !bytes.Contains(body, cancelMethod) || !json.Valid(body) {
		return nil, false
	}
	var params []byte
	method := 0 // 1: the cancel method, 2: another string, 3: not judged
	if !members(body, func(key, val []byte) {
		switch {
		case bytes.EqualFold(key, []byte("method")):
			switch {
			case val[0] != '"' || bytes.IndexByte(val, '\\') >= 0:
				method = 3
			case bytes.Equal(val[1:len(val)-1], cancelMethod):
				method = 1
			default:
				method = 2
			}
		case bytes.EqualFold(key, []byte("params")):
			params = val
		}
	}) || method != 1 {
		return nil, false
	}
	if params != nil {
		members(params, func(key, val []byte) {
			if bytes.EqualFold(key, []byte("id")) {
				id = val
			}
		})
	}
	return id, true
}

// members calls fn for each member of the JSON object obj, in order, with the
// key text and the raw value. obj must be valid JSON; false means it is not an
// object or has a key with an escape (which members does not decode).
func members(obj []byte, fn func(key, val []byte)) bool {
	i := skipSpace(obj, 0)
	if i >= len(obj) || obj[i] != '{' {
		return false
	}
	i = skipSpace(obj, i+1)
	if i < len(obj) && obj[i] == '}' {
		return true
	}
	for {
		if i >= len(obj) || obj[i] != '"' {
			return false
		}
		end := stringEnd(obj, i)
		if end < 0 {
			return false
		}
		key := obj[i+1 : end-1]
		if bytes.IndexByte(key, '\\') >= 0 {
			return false
		}
		i = skipSpace(obj, end)
		if i >= len(obj) || obj[i] != ':' {
			return false
		}
		i = skipSpace(obj, i+1)
		vend := valueEnd(obj, i)
		if vend < 0 {
			return false
		}
		fn(key, obj[i:vend])
		i = skipSpace(obj, vend)
		if i >= len(obj) {
			return false
		}
		if obj[i] == ',' {
			i = skipSpace(obj, i+1)
			continue
		}
		return obj[i] == '}'
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// stringEnd is the index after the closing quote of the string starting at i.
func stringEnd(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// valueEnd is the index after the JSON value starting at i.
func valueEnd(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return stringEnd(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				if j = stringEnd(b, j) - 1; j < 0 {
					return -1
				}
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i
	for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ' ' && b[j] != '\t' && b[j] != '\n' && b[j] != '\r' {
		j++
	}
	return j
}

// ---- loop ----

// serve runs the message loop until `exit` or the end of input.
func (s *server) serve() error {
	go s.readLoop()
	go s.forward()
	defer close(s.done)
	defer func() {
		s.baseCancel() // kills the ShellCheck runs still going
		// A kill is delivered by the run's own goroutine, which then reaps the
		// child; return only once they have, so that a server that exits right
		// after Serve does not leave a ShellCheck behind (on Windows nothing
		// else would end it before its own time limit).
		for deadline := time.Now().Add(5 * time.Second); s.checking.Load() > 0 && time.Now().Before(deadline); {
			time.Sleep(2 * time.Millisecond)
		}
	}()
	for !s.exit {
		in, quit := s.inbound, (<-chan struct{})(nil)
		if len(s.held) >= inboundCap {
			// Enough is held: stop taking input until a folder job ends, unless
			// `exit` has been read. Then take what is queued (the reader has
			// stopped, so it is bounded) to reach the `exit`.
			select {
			case <-s.quit:
			default:
				in, quit = nil, s.quit
			}
		}
		select {
		case <-quit: // re-evaluate: input is taken now
		case it := <-in:
			if err := s.step(it); err != nil {
				return err
			}
		case fn := <-s.posted:
			s.runPosted(fn)
		case <-s.wake:
			s.dropCancelledHeld()
		}
		if err := s.c.writeErr(); err != nil {
			return fmt.Errorf("lsp: writing output: %w", err)
		}
	}
	return s.exitErr
}

// step handles one queued item; a non-nil error ends the session.
func (s *server) step(it inbound) error {
	switch {
	case it.err != nil:
		if errors.Is(it.err, errBadFrame) {
			s.log.Printf("fatal framing error: %v", it.err)
		}
		if it.err == io.EOF {
			return errors.New("lsp: input ended before exit")
		}
		return fmt.Errorf("lsp: reading input: %w", it.err)
	case it.fn != nil:
		s.runPosted(it.fn)
	case it.bad != nil:
		s.c.reply(nil, nil, it.bad)
	default:
		s.dispatch(it.msg)
	}
	return nil
}

// runPosted runs a closure posted to the loop; a panic is logged and the loop
// goes on.
func (s *server) runPosted(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic in a closure posted to the loop: %v\n%s", r, debug.Stack())
		}
	}()
	fn()
}

// dispatch takes one message. While a folder job is pending, requests wait in
// held, in arrival order; once something is held, notifications wait behind it,
// so an answer comes from the state its request arrived into and never from a
// later edit. With nothing held an edit goes through at once. An `exit` goes
// through at once too, unless a held `shutdown` is ahead of it.
func (s *server) dispatch(m message) {
	if m.Method == "" {
		s.response(m)
		return
	}
	isReq := len(m.ID) > 0
	if m.Method == "exit" {
		if len(s.held) > 0 && s.heldShutdown() {
			s.held = append(s.held, m)
			return
		}
	} else if len(s.held) > 0 || isReq && s.folderPending > 0 {
		if isReq && s.isCancelled(idKey(m.ID)) {
			s.answer(m.ID, nil, cancelledError()) // its cancel was read, and its wake-up used, already
			return
		}
		s.held = append(s.held, m)
		return
	}
	s.dispatchNow(m)
}

func (s *server) heldShutdown() bool {
	for _, m := range s.held {
		if m.Method == "shutdown" {
			return true
		}
	}
	return false
}

// dispatchNow runs a message that is not held (or no longer is).
func (s *server) dispatchNow(m message) {
	isReq := len(m.ID) > 0
	if m.Method == "exit" {
		s.exit = true
		if !s.shutdown {
			s.exitErr = errors.New("lsp: exit before shutdown")
		}
		return
	}
	switch {
	case m.Method == "initialize":
		// handled below
	case !s.initialized:
		if isReq {
			s.answer(m.ID, nil, &rpcError{codeNotInitialized, "server not initialized"})
		}
		return
	case s.shutdown:
		if isReq {
			s.answer(m.ID, nil, &rpcError{codeInvalidRequest, "server is shut down"})
		}
		return
	}
	s.run(m)
}

// deferred is what a handler returns when it answers later: it has started work
// off the loop that ends in a closure posted to the loop which calls s.answer
// (once) with the request's id. Handlers read the id and the live entry from
// s.curID and s.cur.
type deferred struct{}

// run runs the handler of m and answers a request. A request cancelled before
// it starts is answered without running; one cancelled while it ran has its
// result replaced.
func (s *server) run(m message) {
	if len(m.ID) == 0 {
		s.safeHandle(m)
		return
	}
	key := idKey(m.ID)
	if s.isCancelled(key) {
		s.answer(m.ID, nil, cancelledError())
		return
	}
	s.cur, s.curID = s.liveOf(key), m.ID
	result, rerr := s.safeHandle(m)
	s.cur, s.curID = nil, nil
	if _, ok := result.(deferred); ok && rerr == nil {
		return // the handler answers later, from the loop, with s.answer
	}
	if s.isCancelled(key) {
		result, rerr = nil, cancelledError()
	}
	s.answer(m.ID, result, rerr)
}

// dropCancelledHeld answers the held requests that were cancelled.
func (s *server) dropCancelledHeld() {
	kept := s.held[:0]
	for _, m := range s.held {
		if len(m.ID) > 0 && s.isCancelled(idKey(m.ID)) {
			s.answer(m.ID, nil, cancelledError())
			continue
		}
		kept = append(kept, m)
	}
	clear(s.held[len(kept):])
	s.held = kept
}

// releaseHeld runs the held items in arrival order. Each meets the state its
// predecessors left: a request after a held shutdown is refused.
func (s *server) releaseHeld() {
	for len(s.held) > 0 && s.folderPending == 0 && !s.exit {
		m := s.held[0]
		s.held[0] = message{}
		s.held = s.held[1:]
		s.dispatchNow(m)
	}
}

// ---- folder worker ----

// queueFolderJob asks the folder worker to reconcile the served folders with
// the declared ones as they are now. Loop only. While a job is queued the loop
// holds requests (see dispatch), so an answer never mixes the folders before
// and after a change.
func (s *server) queueFolderJob() {
	s.folderPending++
	s.folderMu.Lock()
	s.folderQ = append(s.folderQ, slices.Clone(s.declared))
	start := !s.folderStarted
	s.folderStarted = true
	s.folderMu.Unlock()
	if start {
		go s.folderWorker(s.folders) // the worker's own last result starts as what the first build made
	}
	select {
	case s.folderSig <- struct{}{}:
	default:
	}
}

func (s *server) nextFolderJob() ([]declaredFolder, bool) {
	s.folderMu.Lock()
	defer s.folderMu.Unlock()
	if len(s.folderQ) == 0 {
		return nil, false
	}
	job := s.folderQ[0]
	s.folderQ[0] = nil
	s.folderQ = s.folderQ[1:]
	return job, true
}

// folderWorker runs the folder jobs one after another, each over the last
// job's folders, and posts every result to the loop.
func (s *server) folderWorker(cur []*folder) {
	for {
		select {
		case <-s.folderSig:
		case <-s.done:
			return
		}
		for {
			select {
			case <-s.done:
				return
			default:
			}
			job, ok := s.nextFolderJob()
			if !ok {
				break
			}
			cur = s.runFolderJob(job, cur)
			res := cur
			s.post(func() { s.applyFolders(res) })
		}
	}
}

// runFolderJob is reconcile, behind the test hook and a recover: a panic
// leaves the folders as they were.
func (s *server) runFolderJob(declared []declaredFolder, existing []*folder) (out []*folder) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Printf("panic opening workspace folders: %v\n%s", r, debug.Stack())
			out = existing
		}
	}()
	if _, job, _ := hooks(); job != nil {
		job()
	}
	return s.reconcile(declared, existing)
}

// applyFolders installs the result of a folder job, on the loop: it swaps the
// served folders and, when the set changed, re-homes the open documents (see
// rehome). The last job's result releases the held requests.
func (s *server) applyFolders(res []*folder) {
	defer func() { // counted as applied even if re-homing panics
		if s.folderPending--; s.folderPending == 0 {
			s.releaseHeld()
		}
	}()
	old := s.folders
	s.folders = res
	s.dropTools() // the roots the ShellCheck programs are checked against changed
	dropped := map[backend]bool{}
	same := len(old) == len(res)
	for i, f := range old {
		if !slices.Contains(res, f) {
			dropped[f.ws] = true
		}
		same = same && i < len(res) && res[i] == f
	}
	if !same {
		s.epoch++ // a pull id issued under the old folders must not read as current under the new
		s.rehome(dropped)
	}
}
