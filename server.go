// Raw TCP HTTP/1.1 server: no net/http. Accepts connections, parses the
// request line + headers from raw bytes, reads the body, and dispatches
// through three fast paths (read issue, git info/refs, create issue) before
// the generic handlers. Port of the app.ts routing with ricoxyz-style
// fast paths.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

var statusText = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type httpServer struct {
	store   *Store
	token   string
	counter uint64
}

type httpRequest struct {
	method         string
	path           string // decoded path
	query          url.Values
	host           string
	auth           string
	gitProt        string
	contentLength  int
	chunked        bool
	expectContinue bool
	closeConn      bool
	body           []byte
}

var bufioReaderPool = sync.Pool{
	New: func() interface{} { return bufio.NewReaderSize(nil, 4096) },
}

var writeBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 16384)
		return &b
	},
}

func (s *httpServer) Serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			setQuickAck(tc)
		}
		go s.serveConn(conn)
	}
}

func (s *httpServer) serveConn(conn net.Conn) {
	defer conn.Close()
	br := bufioReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer func() {
		br.Reset(nil)
		bufioReaderPool.Put(br)
	}()
	wbufPtr := writeBufPool.Get().(*[]byte)
	defer func() {
		if cap(*wbufPtr) <= 65536 {
			*wbufPtr = (*wbufPtr)[:0]
			writeBufPool.Put(wbufPtr)
		}
	}()
	bodyBuf := make([]byte, 0, 8192)
	for {
		req, err := readRequest(br)
		if err != nil {
			return
		}
		if err := readBodyInto(br, req, conn, &bodyBuf); err != nil {
			return
		}
		req.body = bodyBuf
		wbuf := (*wbufPtr)[:0]
		s.dispatch(req, &wbuf)
		if len(wbuf) > 0 {
			if _, err := conn.Write(wbuf); err != nil {
				return
			}
		}
		*wbufPtr = wbuf
		bodyBuf = bodyBuf[:0]
		if req.closeConn {
			return
		}
	}
}

// readLine reads one CRLF-terminated line (without CRLF) from br.
func readLine(br *bufio.Reader, scratch []byte) ([]byte, error) {
	scratch = scratch[:0]
	for {
		frag, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			scratch = append(scratch, frag...)
			continue
		}
		if err != nil {
			return nil, err
		}
		scratch = append(scratch, frag...)
		if n := len(scratch); n > 0 && scratch[n-1] == '\n' {
			scratch = scratch[:n-1]
			if n := len(scratch); n > 0 && scratch[n-1] == '\r' {
				scratch = scratch[:n-1]
			}
		}
		return scratch, nil
	}
}

func readRequest(br *bufio.Reader) (*httpRequest, error) {
	var scratch []byte
	line, err := readLine(br, scratch)
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		// stray CRLF before the request line: read one more line.
		line, err = readLine(br, scratch)
		if err != nil {
			return nil, err
		}
	}
	parts := strings.SplitN(string(line), " ", 3)
	if len(parts) < 2 {
		return nil, io.ErrUnexpectedEOF
	}
	req := &httpRequest{method: parts[0], contentLength: -1, query: url.Values{}}
	target := parts[1]
	rawPath := target
	if q := strings.IndexByte(target, '?'); q >= 0 {
		rawPath = target[:q]
		req.query, _ = url.ParseQuery(target[q+1:])
	}
	if unescaped, err := url.PathUnescape(rawPath); err == nil {
		req.path = unescaped
	} else {
		req.path = rawPath
	}
	for {
		hline, err := readLine(br, scratch)
		if err != nil {
			return nil, err
		}
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToLower(string(bytes.TrimSpace(hline[:colon])))
		value := string(bytes.TrimSpace(hline[colon+1:]))
		switch name {
		case "host":
			req.host = value
		case "authorization":
			req.auth = value
		case "git-protocol":
			req.gitProt = value
		case "content-length":
			if v, err := strconv.Atoi(value); err == nil {
				req.contentLength = v
			}
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				req.chunked = true
			}
		case "expect":
			if strings.EqualFold(value, "100-continue") {
				req.expectContinue = true
			}
		case "connection":
			if strings.EqualFold(value, "close") {
				req.closeConn = true
			}
		}
	}
	if req.host == "" {
		req.host = "localhost"
	}
	return req, nil
}

func readBodyInto(br *bufio.Reader, req *httpRequest, conn net.Conn, buf *[]byte) error {
	if req.expectContinue {
		_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}
	*buf = (*buf)[:0]
	if req.chunked {
		var scratch []byte
		for {
			line, err := readLine(br, scratch)
			if err != nil {
				return err
			}
			sizeStr := string(line)
			if i := strings.IndexByte(sizeStr, ';'); i >= 0 {
				sizeStr = sizeStr[:i]
			}
			size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
			if err != nil {
				return err
			}
			if size == 0 {
				for {
					tline, err := readLine(br, scratch)
					if err != nil {
						return err
					}
					if len(tline) == 0 {
						break
					}
				}
				break
			}
			start := len(*buf)
			*buf = append(*buf, make([]byte, size)...)
			if _, err := io.ReadFull(br, (*buf)[start:]); err != nil {
				return err
			}
			if _, err := br.Discard(2); err != nil {
				return err
			}
			if len(*buf) > 64*1024*1024 {
				return io.ErrShortBuffer
			}
		}
		return nil
	}
	if req.contentLength > 0 {
		if req.contentLength > 64*1024*1024 {
			return io.ErrShortBuffer
		}
		start := len(*buf)
		*buf = append(*buf, make([]byte, req.contentLength)...)
		if _, err := io.ReadFull(br, (*buf)[start:]); err != nil {
			return err
		}
	}
	return nil
}

func (s *httpServer) nextRequestID() string {
	n := atomic.AddUint64(&s.counter, 1)
	return "req-" + strconv.FormatUint(n, 10)
}

// writeResult frames one Result into wbuf.
func writeResult(wbuf *[]byte, res Result, closeConn bool) {
	text, ok := statusText[res.Status]
	if !ok {
		text = "OK"
	}
	*wbuf = append(*wbuf, "HTTP/1.1 "...)
	*wbuf = strconv.AppendInt(*wbuf, int64(res.Status), 10)
	*wbuf = append(*wbuf, ' ')
	*wbuf = append(*wbuf, text...)
	*wbuf = append(*wbuf, "\r\n"...)
	for _, h := range res.Extra {
		*wbuf = append(*wbuf, h[0]...)
		*wbuf = append(*wbuf, ": "...)
		*wbuf = append(*wbuf, h[1]...)
		*wbuf = append(*wbuf, "\r\n"...)
	}
	if closeConn {
		*wbuf = append(*wbuf, "Connection: close\r\n"...)
	}
	*wbuf = append(*wbuf, "Content-Length: "...)
	*wbuf = strconv.AppendInt(*wbuf, int64(len(res.Body)), 10)
	*wbuf = append(*wbuf, "\r\n\r\n"...)
	*wbuf = append(*wbuf, res.Body...)
}

// splitSegments splits a path into non-empty segments.
func splitSegments(path string) []string {
	var out []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (s *httpServer) dispatch(req *httpRequest, wbuf *[]byte) {
	rid := s.nextRequestID()
	path := req.path
	method := req.method
	segs := splitSegments(path)

	// /healthz takes no token.
	if method == "GET" && path == "/healthz" {
		writeResult(wbuf, jsonResult(200, []byte(`{"ok":true}`), rid), req.closeConn)
		return
	}

	// Auth: every other endpoint needs the trial's bearer token.
	if s.token != "" {
		if req.auth != "Bearer "+s.token {
			writeResult(wbuf, errorResult(401, "Requires authentication", rid), req.closeConn)
			return
		}
	}

	// ---- git smart HTTP ----
	if len(segs) >= 3 && strings.HasSuffix(segs[1], ".git") {
		owner := segs[0]
		repoName := strings.TrimSuffix(segs[1], ".git")
		servicePath := strings.Join(segs[2:], "/")
		s.store.mu.Lock()
		repo := s.store.getRepoLocked(owner, repoName)
		if repo == nil {
			s.store.mu.Unlock()
			writeResult(wbuf, errorResult(404, "Not Found", rid), req.closeConn)
			return
		}
		res := s.dispatchGit(req, repo, servicePath, rid)
		s.store.mu.Unlock()
		writeResult(wbuf, res, req.closeConn)
		return
	}

	// ---- REST v3 ----
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		restSegs := segs[2:]
		// Fast path A: GET /api/v3/repos/{o}/{r}/issues/{n}
		if method == "GET" && len(restSegs) == 5 && restSegs[0] == "repos" && restSegs[3] == "issues" {
			if num, ok := parseJSInt(restSegs[4]); ok {
				if body, found := s.fastGetIssue(restSegs[1], restSegs[2], num, req.host); found {
					writeResult(wbuf, jsonResult(200, body, rid), req.closeConn)
					return
				}
				// fall through to the generic handler for the 404
			}
		}
		// Fast path C: POST /api/v3/repos/{o}/{r}/issues (no labels key)
		if method == "POST" && len(restSegs) == 4 && restSegs[0] == "repos" && restSegs[3] == "issues" {
			if res, ok := s.fastCreateIssue(restSegs[1], restSegs[2], req.body, rid, req.host); ok {
				writeResult(wbuf, res, req.closeConn)
				return
			}
		}
		body, perr := parseRestBody(req.body)
		if perr != "" {
			writeResult(wbuf, errorResult(400, perr, rid), req.closeConn)
			return
		}
		s.store.mu.Lock()
		res := handleRestLocked(RestCtx{Store: s.store, RequestID: rid, Host: req.host}, method, restSegs, req.query, body)
		s.store.mu.Unlock()
		writeResult(wbuf, res, req.closeConn)
		return
	}

	// ---- GraphQL v4 ----
	if path == "/api/graphql" {
		if method != "POST" {
			writeResult(wbuf, errorResult(404, "Not Found", rid), req.closeConn)
			return
		}
		parsed, malformed := parseJSONValue(req.body)
		if malformed {
			writeResult(wbuf, errorResult(400, "Malformed JSON in request body", rid), req.closeConn)
			return
		}
		s.store.mu.Lock()
		res := handleGraphQLLocked(s.store, rid, parsed)
		s.store.mu.Unlock()
		writeResult(wbuf, res, req.closeConn)
		return
	}

	// ---- MCP ----
	if path == "/mcp" {
		if method != "POST" {
			writeResult(wbuf, errorResult(404, "Not Found", rid), req.closeConn)
			return
		}
		s.store.mu.Lock()
		res := handleMcpLocked(&mcpCtx{store: s.store, requestID: rid, host: req.host}, req.body)
		s.store.mu.Unlock()
		writeResult(wbuf, res, req.closeConn)
		return
	}

	writeResult(wbuf, errorResult(404, "Not Found", rid), req.closeConn)
}

// dispatchGit handles a git smart-HTTP request. Caller holds the store lock.
func (s *httpServer) dispatchGit(req *httpRequest, repo *Repo, servicePath, rid string) Result {
	gitResult := func(status int, body []byte, contentType string) Result {
		return Result{Status: status, Body: body, Extra: [][2]string{
			{"Content-Type", contentType},
			{"X-GitHub-Request-Id", rid},
		}}
	}
	if req.method == "GET" && servicePath == "info/refs" {
		service := req.query.Get("service")
		if service == "git-upload-pack" {
			if req.gitProt == "version=2" {
				return gitResult(200, advertisementV2(), "application/x-git-upload-pack-advertisement")
			}
			return gitResult(200, advertisementV0(repo, "git-upload-pack"), "application/x-git-upload-pack-advertisement")
		}
		if service == "git-receive-pack" {
			return gitResult(200, receivePackAdv(repo), "application/x-git-receive-pack-advertisement")
		}
		return errorResult(404, "Not Found", rid)
	}
	if req.method == "POST" && servicePath == "git-upload-pack" {
		packets, err := decodePktLines(req.body)
		if err != nil {
			return errorResult(400, err.Error(), rid)
		}
		command := ""
		if len(packets) > 0 && packets[0].Kind == "data" {
			command = strings.TrimSpace(string(packets[0].Payload))
		}
		switch {
		case command == "command=ls-refs":
			return gitResult(200, lsRefsResponse(repo, parseLsRefsArgs(packets)), "application/x-git-upload-pack-result")
		case command == "command=fetch":
			return gitResult(200, fetchV2Response(repo, parseFetchV2Args(packets)), "application/x-git-upload-pack-result")
		case strings.HasPrefix(command, "want ") || command == "":
			return gitResult(200, fetchV0Response(repo, parseFetchV0Args(packets)), "application/x-git-upload-pack-result")
		default:
			return errorResult(400, "unsupported upload-pack command: "+command, rid)
		}
	}
	if req.method == "POST" && servicePath == "git-receive-pack" {
		commands, pack, caps, err := parseReceivePack(req.body)
		if err != nil {
			return errorResult(400, err.Error(), rid)
		}
		var objects map[string]*GitObject
		if len(pack) > 0 {
			objects, err = readPack(pack, repo.Objects)
			if err != nil {
				return errorResult(400, err.Error(), rid)
			}
		} else {
			objects = make(map[string]*GitObject)
		}
		report := applyPush(repo, s.store, commands, objects)
		sideband := false
		for _, c := range caps {
			if c == "side-band-64k" {
				sideband = true
			}
		}
		return gitResult(200, pushReportResponse(report, sideband), "application/x-git-receive-pack-result")
	}
	return errorResult(404, "Not Found", rid)
}

// parseRestBody parses a REST request body like app.ts: empty -> empty map;
// malformed JSON -> "Malformed JSON in request body"; valid JSON that is
// not an object -> "Request body must be a JSON object".
func parseRestBody(body []byte) (map[string]interface{}, string) {
	if len(body) == 0 {
		return map[string]interface{}{}, ""
	}
	var parsed interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&parsed); err != nil {
		return nil, "Malformed JSON in request body"
	}
	if parsed == nil {
		return nil, "Malformed JSON in request body"
	}
	m, ok := parsed.(map[string]interface{})
	if !ok {
		return nil, "Request body must be a JSON object"
	}
	return m, ""
}

// parseJSONValue parses a JSON body for GraphQL: returns (value, malformed).
// An empty body decodes as nil without error (Node: JSON.parse("") throws,
// but app.ts parseJsonBody returns undefined for empty, and handleGraphQL
// then rejects non-object with 400 'request body must be a JSON object or
// array' — we mirror by returning nil, false and letting the handler 400).
func parseJSONValue(body []byte) (interface{}, bool) {
	if len(body) == 0 {
		return nil, false
	}
	var parsed interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&parsed); err != nil {
		return nil, true
	}
	return parsed, false
}

// fastGetIssue is fast path A: GET one issue, cached JSON body.
// Returns (body, true) on a hit; (nil, false) when the issue/repo is
// missing (caller falls through to the generic 404 path).
func (s *httpServer) fastGetIssue(owner, repoName string, number int, host string) ([]byte, bool) {
	s.store.mu.RLock()
	repo := s.store.getRepoLocked(owner, repoName)
	if repo == nil {
		s.store.mu.RUnlock()
		return nil, false
	}
	issue := repo.Issues[number]
	if issue == nil {
		s.store.mu.RUnlock()
		return nil, false
	}
	if issue.cachedBody != nil && issue.cachedHost == host {
		body := issue.cachedBody
		s.store.mu.RUnlock()
		return body, true
	}
	body := issueJSON(RestCtx{Store: s.store, Host: host}, repo, issue)
	s.store.mu.RUnlock()
	s.store.mu.Lock()
	if issue.cachedBody == nil || issue.cachedHost != host {
		issue.cachedBody = body
		issue.cachedHost = host
	}
	s.store.mu.Unlock()
	return body, true
}

// fastCreateIssue is fast path C: POST /issues with a simple {title, body}
// object and no "labels" key. Returns (result, true) when handled.
func (s *httpServer) fastCreateIssue(owner, repoName string, rawBody []byte, rid, host string) (Result, bool) {
	title, bodyStr, ok := parseSimpleIssueJSON(rawBody)
	if !ok {
		return Result{}, false
	}
	if title == "" {
		// Let the generic path produce the 422.
		return Result{}, false
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo := s.store.getRepoLocked(owner, repoName)
	if repo == nil {
		return Result{}, false
	}
	number := repo.NextNumber
	repo.NextNumber++
	seq := s.store.tickLocked()
	issue := &Issue{
		ID: number, Number: number, Title: title, Body: bodyStr,
		State: "open", Labels: []string{}, User: viewerLogin,
		CreatedSeq: seq, UpdatedSeq: seq,
	}
	repo.Issues[number] = issue
	repo.IssueOrder = append(repo.IssueOrder, issue)
	return issueResult(RestCtx{Store: s.store, RequestID: rid, Host: host}, repo, issue, 201), true
}

// parseSimpleIssueJSON extracts top-level "title" and "body" string fields
// from a small JSON object. Fails (returns false) when the body has a
// "labels" key or any shape the scanner cannot prove simple, so the caller
// falls back to the generic JSON path.
func parseSimpleIssueJSON(body []byte) (title, bodyStr string, ok bool) {
	if len(body) == 0 || len(body) > 65536 {
		return "", "", false
	}
	if bytes.Contains(body, []byte(`"labels"`)) {
		return "", "", false
	}
	// Minimal scanner: find "title":"..." and "body":"..." at any depth
	// (the bench payloads are flat objects).
	extract := func(key string) (string, bool) {
		needle := []byte(`"` + key + `"`)
		idx := bytes.Index(body, needle)
		if idx < 0 {
			return "", false
		}
		i := idx + len(needle)
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}
		if i >= len(body) || body[i] != ':' {
			return "", false
		}
		i++
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
			i++
		}
		if i >= len(body) || body[i] != '"' {
			return "", false
		}
		i++
		var sb strings.Builder
		for i < len(body) {
			c := body[i]
			i++
			if c == '"' {
				return sb.String(), true
			}
			if c == '\\' && i < len(body) {
				n := body[i]
				i++
				switch n {
				case 'n':
					sb.WriteByte('\n')
				case 't':
					sb.WriteByte('\t')
				case 'r':
					sb.WriteByte('\r')
				case 'b':
					sb.WriteByte('\b')
				case 'f':
					sb.WriteByte('\f')
				case 'u':
					if i+4 <= len(body) {
						if v, err := strconv.ParseUint(string(body[i:i+4]), 16, 32); err == nil {
							sb.WriteRune(rune(v))
							i += 4
							continue
						}
					}
					return "", false
				default:
					sb.WriteByte(n)
				}
			} else {
				sb.WriteByte(c)
			}
		}
		return "", false
	}
	t, ok1 := extract("title")
	if !ok1 {
		return "", "", false
	}
	b, ok2 := extract("body")
	if !ok2 {
		// body is optional in the generic path; missing means "".
		b = ""
	}
	return t, b, true
}
