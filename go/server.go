// The forge's HTTP server: routing, auth, JSON plumbing. Surfaces:
//   GET  /healthz                     200 {"ok":true} (no auth)
//   /{owner}/{repo}.git/...           git smart HTTP (no auth check)
//   /api/v3/...                       GitHub REST v3 (Bearer required)
//   POST /api/graphql                 GraphQL v4 (Bearer required)
//   POST /mcp                         MCP streamable HTTP (Bearer required)
//   anything else                     404 {"message":"Not Found"}
//
// Raw TCP HTTP/1.1 transport: one goroutine per connection, requests
// dispatched strictly in order, one write per reply, TCP_NODELAY.
// Dispatch runs under a single mutex (the store is not goroutine-safe);
// the mutex is released before the socket write. Ported from the Node
// app.ts raw-socket transport; behavior is identical.

package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"
const maxBodyBytes = 64 * 1024 * 1024

var reasons = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

// ForgeServer is a listening forge server.
type ForgeServer struct {
	store    *Store
	listener net.Listener
	mu       sync.Mutex // guards dispatch (store access) and the memo
	closed   atomic.Bool
	wg       sync.WaitGroup
	conns    sync.Map // net.Conn -> struct{}

	cachedBearer string
	bearerInit   bool
	reqSeq       atomic.Int64

	// GET memo
	memo      map[string]*memoEntry
	memoOrder []string
}

type memoEntry struct {
	status        int
	body          []byte
	contentType   string
	headers       map[string]string
	head          []byte
	tailKeepAlive []byte
	tailClose     []byte
}

// RouteResult is one finished response: status, exact bytes, content type,
// extra headers. The request id and content length are added by the
// transport at send time, never stored here, so a memoized answer can be
// re-sent with a fresh id.
type RouteResult struct {
	Status      int
	Body        []byte
	ContentType string
	Headers     map[string]string
	// PrebuiltHead, when set, is the constant head bytes baked at cache time.
	PrebuiltHead *prebuiltHead
}

type prebuiltHead struct {
	head          []byte
	tailKeepAlive []byte
	tailClose     []byte
}

func jsonResult(status int, body *JVal, headers map[string]string) RouteResult {
	var text []byte
	if body != nil {
		text = append([]byte(body.Marshal()), '\n')
	} else {
		text = []byte{}
	}
	return RouteResult{Status: status, Body: text, ContentType: jsonType, Headers: headers}
}

func textResult(status int, text string, headers map[string]string) RouteResult {
	return RouteResult{Status: status, Body: []byte(text + "\n"), ContentType: jsonType, Headers: headers}
}

func rawResult(status int, body []byte, contentType string, headers map[string]string) RouteResult {
	return RouteResult{Status: status, Body: body, ContentType: contentType, Headers: headers}
}

func emptyResult(status int, headers map[string]string) RouteResult {
	return RouteResult{Status: status, Body: []byte{}, ContentType: jsonType, Headers: headers}
}

// NewForgeServer creates a server bound to the given store.
func NewForgeServer(store *Store) *ForgeServer {
	return &ForgeServer{store: store, memo: map[string]*memoEntry{}}
}

func (s *ForgeServer) requestID() string {
	return "req-" + strconv.FormatInt(s.reqSeq.Add(1), 10)
}

func (s *ForgeServer) authOK(headers map[string]string) bool {
	if !s.bearerInit {
		s.cachedBearer = "Bearer " + os.Getenv("MAKEFUN_SHOP_TOKEN")
		s.bearerInit = true
	}
	if s.cachedBearer == "Bearer " {
		return true
	}
	return headers["authorization"] == s.cachedBearer
}

// Listen binds to the given address (use ":0" for an ephemeral port).
func (s *ForgeServer) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	s.wg.Add(1)
	go s.acceptLoop()
	return nil
}

// Addr returns the bound port.
func (s *ForgeServer) Addr() int {
	if s.listener == nil {
		return 0
	}
	if ta, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// Close shuts the listener and all open connections down.
func (s *ForgeServer) Close() {
	s.closed.Store(true)
	if s.listener != nil {
		s.listener.Close()
	}
	s.conns.Range(func(k, _ any) bool {
		if c, ok := k.(net.Conn); ok {
			c.Close()
		}
		return true
	})
	s.wg.Wait()
}

func (s *ForgeServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}
		s.conns.Store(conn, struct{}{})
		s.wg.Add(1)
		go s.serveConn(conn)
	}
}

// ---------------------------------------------------------------- GET memo

const getMemoMaxEntries = 1024
const getMemoMaxBody = 1024 * 1024

func (s *ForgeServer) memoKey(url string, host string, authorized bool) string {
	auth := "0"
	if authorized {
		auth = "1"
	}
	return strconv.Itoa(s.store.Version) + "\n" + auth + "\n" + host + "\n" + url
}

// dispatch routes one request, memoizing GET answers under /api/v3.
// Caller must hold s.mu; the returned RouteResult is safe to send after
// the mutex is released.
func (s *ForgeServer) dispatch(method, rawURL string, headers map[string]string, body []byte) RouteResult {
	isGet := method == "GET"
	memoable := isGet && strings.HasPrefix(rawURL, "/api/v3/")
	authorized := s.authOK(headers)
	var key string
	if memoable {
		key = s.memoKey(rawURL, headers["host"], authorized)
		if hit, ok := s.memo[key]; ok {
			return RouteResult{
				Status:      hit.status,
				Body:        hit.body,
				ContentType: hit.contentType,
				Headers:     hit.headers,
				PrebuiltHead: &prebuiltHead{
					head:          hit.head,
					tailKeepAlive: hit.tailKeepAlive,
					tailClose:     hit.tailClose,
				},
			}
		}
	}
	result := s.handle(method, rawURL, headers, body, authorized)
	if memoable && len(result.Body) <= getMemoMaxBody {
		if len(s.memo) >= getMemoMaxEntries {
			oldest := s.memoOrder[0]
			s.memoOrder = s.memoOrder[1:]
			delete(s.memo, oldest)
		}
		tpl := headTemplate(result.Status, result.ContentType, result.Headers)
		head := make([]byte, 0, len(tpl.start)+10+len(tpl.mid))
		head = append(head, tpl.start...)
		head = append(head, []byte(strconv.Itoa(len(result.Body)))...)
		head = append(head, tpl.mid...)
		s.memo[key] = &memoEntry{
			status:        result.Status,
			body:          result.Body,
			contentType:   result.ContentType,
			headers:       result.Headers,
			head:          head,
			tailKeepAlive: tpl.endKeepAlive,
			tailClose:     tpl.endClose,
		}
		s.memoOrder = append(s.memoOrder, key)
	}
	if !isGet {
		s.store.BumpVersion()
	}
	return result
}

// ---------------------------------------------------------------- routing

// queryParam reads one query parameter out of a raw query string without
// building a URL object — the info/refs service check is the only git caller.
func queryParam(queryStr, name string) string {
	needle := name + "="
	i := strings.Index(queryStr, needle)
	if i == -1 || (i > 0 && queryStr[i-1] != '&') {
		return ""
	}
	start := i + len(needle)
	amp := strings.IndexByte(queryStr[start:], '&')
	var raw string
	if amp == -1 {
		raw = queryStr[start:]
	} else {
		raw = queryStr[start : start+amp]
	}
	if dec, err := url.QueryUnescape(strings.ReplaceAll(raw, "+", " ")); err == nil {
		return dec
	}
	return raw
}

func (s *ForgeServer) handle(method, rawURL string, headers map[string]string, body []byte, authorized bool) RouteResult {
	q := strings.IndexByte(rawURL, '?')
	path := rawURL
	queryStr := ""
	if q != -1 {
		path = rawURL[:q]
		queryStr = rawURL[q+1:]
	}
	var queryCache url.Values
	queryParams := func() url.Values {
		if queryCache == nil {
			queryCache, _ = url.ParseQuery(queryStr)
		}
		return queryCache
	}
	segs := []string{}
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}

	// Health probe: no auth.
	if method == "GET" && path == "/healthz" {
		return jsonResult(200, JObj(JField("ok", JBool(true))), nil)
	}

	// Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack
	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		owner, _ := url.PathUnescape(segs[0])
		repoName, _ := url.PathUnescape(segs[1][:len(segs[1])-len(".git")])
		sub := strings.Join(segs[2:], "/")
		repo := s.store.GetRepo(owner, repoName)
		if sub == "info/refs" {
			service := queryParam(queryStr, "service")
			if method != "GET" || repo == nil || (service != "git-upload-pack" && service != "git-receive-pack") {
				return jsonResult(404, JObj(JField("message", "Not Found")), nil)
			}
			isV2 := headers["git-protocol"] == "version=2"
			if service == "git-upload-pack" && isV2 {
				return rawResult(200, advertiseV2(), "application/x-git-upload-pack-advertisement", nil)
			}
			return rawResult(200, advertisedV0(s.store, repo, service), "application/x-"+service+"-advertisement", nil)
		}
		if sub == "git-upload-pack" && method == "POST" {
			if repo == nil {
				return jsonResult(404, JObj(JField("message", "Not Found")), nil)
			}
			out := handleUploadPack(s.store, repo, body, headers["git-protocol"] == "version=2")
			return rawResult(out.status, out.body, out.contentType, nil)
		}
		if sub == "git-receive-pack" && method == "POST" {
			if repo == nil {
				return jsonResult(404, JObj(JField("message", "Not Found")), nil)
			}
			out := handleReceivePack(s.store, repo, body)
			return rawResult(out.status, out.body, out.contentType, nil)
		}
		return jsonResult(404, JObj(JField("message", "Not Found")), nil)
	}

	// Authenticated surfaces.
	if path == "/api/graphql" || path == "/mcp" || path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		if !authorized {
			return jsonResult(401, JObj(JField("message", "Requires authentication")), nil)
		}
	}

	// API root.
	if method == "GET" && (path == "/api/v3" || path == "/api/v3/") {
		return jsonResult(200, JObj(
			JField("current_user_url", "/api/v3/user"),
			JField("rate_limit_url", "/api/v3/rate_limit"),
		), map[string]string{"x-oauth-scopes": "repo, read:org"})
	}

	// GraphQL.
	if path == "/api/graphql" {
		if method != "POST" {
			return jsonResult(404, JObj(JField("message", "Not Found")), nil)
		}
		var parsed any
		if len(body) == 0 {
			parsed = map[string]any{}
		} else if err := json.Unmarshal(body, &parsed); err != nil {
			return jsonResult(400, JObj(JField("errors", JArr(JObj(JField("message", "Parse error"))))), nil)
		}
		result := executeGraphQL(s.store, parsed)
		return jsonResult(200, result, nil)
	}

	// MCP.
	if path == "/mcp" {
		if method == "DELETE" {
			return emptyResult(204, nil)
		}
		if method != "POST" {
			return jsonResult(404, JObj(JField("message", "Not Found")), nil)
		}
		out := handleMcp(s.store, body)
		if out.Body == nil {
			return emptyResult(out.Status, out.Headers)
		}
		return jsonResult(out.Status, out.Body, out.Headers)
	}

	// REST v3.
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		var restSegs []string
		if path != "/api/v3" {
			for _, seg := range strings.Split(path[len("/api/v3/"):], "/") {
				if seg != "" {
					restSegs = append(restSegs, seg)
				}
			}
		}
		var parsedBody map[string]any
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			if len(body) > 0 {
				var v any
				if err := json.Unmarshal(body, &v); err != nil {
					return jsonResult(400, JObj(JField("message", "Invalid JSON")), nil)
				}
				if m, ok := v.(map[string]any); ok {
					parsedBody = m
				} else {
					parsedBody = map[string]any{}
				}
			}
		}
		host := headers["host"]
		if host == "" {
			host = "entry"
		}
		baseURL := "http://" + host
		out := handleRest(RestCtx{
			Store:    s.store,
			Query:    queryParams(),
			RawQuery: queryStr,
			Body:     parsedBody,
			BaseURL:  baseURL,
		}, method, restSegs)
		if out.Status == 204 {
			return emptyResult(204, out.Headers)
		}
		if out.HasText {
			return textResult(out.Status, out.Text, out.Headers)
		}
		return jsonResult(out.Status, out.Body, out.Headers)
	}

	return jsonResult(404, JObj(JField("message", "Not Found")), nil)
}

// ---------------------------------------------------------------- head templates

type headTemplateT struct {
	start        []byte
	mid          []byte
	endKeepAlive []byte
	endClose     []byte
}

var headTemplatesMu sync.Mutex
var headTemplates = map[string]*headTemplateT{}

func headTemplate(status int, contentType string, headers map[string]string) *headTemplateT {
	var keyBuf strings.Builder
	keyBuf.WriteString(strconv.Itoa(status))
	keyBuf.WriteByte('|')
	keyBuf.WriteString(contentType)
	keyBuf.WriteByte('|')
	// Header order matters for the key only; iterate in map order is fine
	// for identity, but the template text must be deterministic — so sort.
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		keyBuf.WriteString(k)
		keyBuf.WriteByte(':')
		keyBuf.WriteString(headers[k])
		keyBuf.WriteByte('|')
	}
	key := keyBuf.String()
	headTemplatesMu.Lock()
	if hit, ok := headTemplates[key]; ok {
		headTemplatesMu.Unlock()
		return hit
	}
	headTemplatesMu.Unlock()

	reason := reasons[status]
	if reason == "" {
		reason = "OK"
	}
	var extraText strings.Builder
	for _, k := range keys {
		extraText.WriteString(k)
		extraText.WriteString(": ")
		extraText.WriteString(headers[k])
		extraText.WriteString("\r\n")
	}
	var start, mid []byte
	if status == 204 {
		start = []byte("HTTP/1.1 204 No Content\r\nx-github-request-id: ")
		mid = []byte{}
	} else {
		start = []byte("HTTP/1.1 " + strconv.Itoa(status) + " " + reason + "\r\ncontent-type: " + contentType + "\r\ncontent-length: ")
		mid = []byte("\r\nx-github-request-id: ")
	}
	tpl := &headTemplateT{
		start:        start,
		mid:          mid,
		endKeepAlive: []byte("\r\nconnection: keep-alive\r\n" + extraText.String() + "\r\n"),
		endClose:     []byte("\r\nconnection: close\r\n" + extraText.String() + "\r\n"),
	}
	headTemplatesMu.Lock()
	if len(headTemplates) < 256 {
		headTemplates[key] = tpl
	}
	headTemplatesMu.Unlock()
	return tpl
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// writeRawResult sends one finished response as a single socket write.
func (s *ForgeServer) writeRawResult(conn net.Conn, result RouteResult, keepAlive bool) {
	id := []byte(s.requestID())
	var out []byte
	if result.PrebuiltHead != nil {
		tail := result.PrebuiltHead.tailKeepAlive
		if !keepAlive {
			tail = result.PrebuiltHead.tailClose
		}
		out = make([]byte, 0, len(result.PrebuiltHead.head)+len(id)+len(tail)+len(result.Body))
		out = append(out, result.PrebuiltHead.head...)
		out = append(out, id...)
		out = append(out, tail...)
		out = append(out, result.Body...)
	} else {
		tpl := headTemplate(result.Status, result.ContentType, result.Headers)
		end := tpl.endKeepAlive
		if !keepAlive {
			end = tpl.endClose
		}
		if result.Status == 204 {
			out = make([]byte, 0, len(tpl.start)+len(id)+len(end))
			out = append(out, tpl.start...)
			out = append(out, id...)
			out = append(out, end...)
		} else {
			out = make([]byte, 0, len(tpl.start)+10+len(tpl.mid)+len(id)+len(end)+len(result.Body))
			out = append(out, tpl.start...)
			out = append(out, []byte(strconv.Itoa(len(result.Body)))...)
			out = append(out, tpl.mid...)
			out = append(out, id...)
			out = append(out, end...)
			out = append(out, result.Body...)
		}
	}
	conn.Write(out)
}

// ---------------------------------------------------------------- raw-socket parsing

func keepAliveFor(version string, headers map[string]string) bool {
	conn := strings.ToLower(headers["connection"])
	if version == "HTTP/1.0" {
		return conn == "keep-alive"
	}
	return conn != "close"
}

type parseOut struct {
	kind           string // need-more, bad, request
	headersDone    bool
	expectContinue bool
	method         string
	url            string
	headers        map[string]string
	body           []byte
	consumed       int
	keepAlive      bool
}

// parseChunked parses one chunked body starting at bodyStart; returns
// (body, consumed, ok) — ok=false means bad, consumed=-1 means incomplete.
func parseChunked(buf []byte, bodyStart int) ([]byte, int, bool) {
	var chunks [][]byte
	pos := bodyStart
	for {
		lineEnd := bytes.Index(buf[pos:], []byte("\r\n"))
		if lineEnd == -1 {
			return nil, -1, true
		}
		lineEnd += pos
		sizeText := strings.TrimSpace(string(buf[pos:lineEnd]))
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			return nil, 0, false
		}
		dataStart := lineEnd + 2
		if size == 0 {
			t := dataStart
			for {
				e := bytes.Index(buf[t:], []byte("\r\n"))
				if e == -1 {
					return nil, -1, true
				}
				e += t
				if e == t {
					var body []byte
					for _, c := range chunks {
						body = append(body, c...)
					}
					return body, e + 2, true
				}
				t = e + 2
			}
		}
		if len(buf) < dataStart+int(size)+2 {
			return nil, -1, true
		}
		chunk := make([]byte, size)
		copy(chunk, buf[dataStart:dataStart+int(size)])
		chunks = append(chunks, chunk)
		pos = dataStart + int(size) + 2
	}
}

// parseRequest parses the first request out of buf, if complete.
func parseRequest(buf []byte) parseOut {
	headEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headEnd == -1 {
		if len(buf) > 65536 {
			return parseOut{kind: "bad"}
		}
		return parseOut{kind: "need-more"}
	}
	lineEnd0 := bytes.Index(buf, []byte("\r\n"))
	if lineEnd0 == -1 || lineEnd0 > headEnd {
		return parseOut{kind: "bad"}
	}
	sp1 := bytes.IndexByte(buf[:lineEnd0], ' ')
	sp2 := -1
	if sp1 != -1 {
		if i := bytes.IndexByte(buf[sp1+1:lineEnd0], ' '); i != -1 {
			sp2 = sp1 + 1 + i
		}
	}
	if sp1 == -1 || sp2 == -1 || sp2 > lineEnd0 {
		return parseOut{kind: "bad"}
	}
	method := string(buf[:sp1])
	rawURL := string(buf[sp1+1 : sp2])
	version := string(buf[sp2+1 : lineEnd0])
	if !strings.HasPrefix(version, "HTTP/") {
		return parseOut{kind: "bad"}
	}
	headers := map[string]string{}
	pos := lineEnd0 + 2
	for pos < headEnd {
		eol := bytes.Index(buf[pos:], []byte("\r\n"))
		if eol == -1 {
			return parseOut{kind: "bad"}
		}
		eol += pos
		if eol > headEnd {
			return parseOut{kind: "bad"}
		}
		colon := bytes.IndexByte(buf[pos:eol], ':')
		if colon == -1 {
			return parseOut{kind: "bad"}
		}
		colon += pos
		if colon > eol {
			return parseOut{kind: "bad"}
		}
		name := strings.TrimSpace(strings.ToLower(string(buf[pos:colon])))
		value := strings.TrimSpace(string(buf[colon+1 : eol]))
		if prev, ok := headers[name]; ok {
			headers[name] = prev + ", " + value
		} else {
			headers[name] = value
		}
		pos = eol + 2
	}
	expectContinue := strings.ToLower(headers["expect"]) == "100-continue"
	bodyStart := headEnd + 4
	te := headers["transfer-encoding"]
	if strings.Contains(strings.ToLower(te), "chunked") {
		body, consumed, ok := parseChunked(buf, bodyStart)
		if !ok {
			return parseOut{kind: "bad"}
		}
		if consumed == -1 {
			return parseOut{kind: "need-more", headersDone: true, expectContinue: expectContinue}
		}
		return parseOut{kind: "request", method: method, url: rawURL, headers: headers, body: body, consumed: consumed, keepAlive: keepAliveFor(version, headers)}
	}
	if cl, ok := headers["content-length"]; ok {
		length, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || length < 0 {
			return parseOut{kind: "bad"}
		}
		if len(buf) < bodyStart+length {
			return parseOut{kind: "need-more", headersDone: true, expectContinue: expectContinue}
		}
		body := make([]byte, length)
		copy(body, buf[bodyStart:bodyStart+length])
		return parseOut{kind: "request", method: method, url: rawURL, headers: headers, body: body, consumed: bodyStart + length, keepAlive: keepAliveFor(version, headers)}
	}
	return parseOut{kind: "request", method: method, url: rawURL, headers: headers, body: []byte{}, consumed: bodyStart, keepAlive: keepAliveFor(version, headers)}
}

// serveConn drains buffered requests on one connection, strictly in order.
func (s *ForgeServer) serveConn(conn net.Conn) {
	defer func() {
		s.conns.Delete(conn)
		conn.Close()
		s.wg.Done()
	}()
	var buf []byte
	scratch := make([]byte, 65536)
	continueSent := false
	for {
		// Pump any complete requests already buffered.
		for {
			parsed := parseRequest(buf)
			if parsed.kind == "need-more" {
				if parsed.headersDone && parsed.expectContinue && !continueSent {
					conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
					continueSent = true
				}
				break
			}
			if parsed.kind == "bad" {
				conn.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
				return
			}
			buf = buf[parsed.consumed:]
			continueSent = false
			// Compact the buffer occasionally so it does not grow unboundedly.
			if len(buf) == 0 {
				buf = nil
			}
			s.mu.Lock()
			var result RouteResult
			func() {
				defer func() {
					if r := recover(); r != nil {
						msg := "Internal Server Error"
						if e, ok := r.(error); ok {
							msg = e.Error()
						} else if str, ok := r.(string); ok {
							msg = str
						}
						result = jsonResult(500, JObj(JField("message", msg)), nil)
					}
				}()
				result = s.dispatch(parsed.method, parsed.url, parsed.headers, parsed.body)
			}()
			s.mu.Unlock()
			s.writeRawResult(conn, result, parsed.keepAlive)
			if !parsed.keepAlive {
				return
			}
			if len(buf) == 0 {
				break
			}
		}
		n, err := conn.Read(scratch)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
		}
		if err != nil {
			return
		}
		if len(buf) > maxBodyBytes+65536 {
			return
		}
	}
}

