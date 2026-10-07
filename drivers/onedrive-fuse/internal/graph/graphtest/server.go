// Package graphtest provides an in-memory fake of the Microsoft Graph
// OneDrive and identity-platform endpoints used by onedrive-fuse.
package graphtest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type item struct {
	id, name, parent string
	dir              bool
	data             []byte
	ctag             int
	mtime            time.Time
}

type session struct {
	parent, name string
	buf          bytes.Buffer
	size         int64
}

// Server is a fake Graph API. GraphURL() is the base for graph.New and
// AuthURL() the base for auth.OAuth.
type Server struct {
	*httptest.Server

	PageSize       int
	TokenExpiresIn int
	QuotaTotal     int64

	mu       sync.Mutex
	items    map[string]*item
	rootID   string
	nextID   int
	tokens   map[string]bool
	refresh  map[string]bool
	sessions map[string]*session
	Requests map[string]int // method -> count, for assertions
	offline  bool
}

// New starts a fake server on a random loopback port.
func New() *Server {
	s := newServer()
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// NewAt starts a fake server listening on addr (e.g. "127.0.0.1:8765").
func NewAt(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := newServer()
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	s.Server.Listener.Close()
	s.Server.Listener = ln
	s.Server.Start()
	return s, nil
}

func newServer() *Server {
	s := &Server{
		PageSize:       1000,
		TokenExpiresIn: 3600,
		QuotaTotal:     5 << 30,
		items:          map[string]*item{},
		tokens:         map[string]bool{},
		refresh:        map[string]bool{},
		sessions:       map[string]*session{},
		Requests:       map[string]int{},
	}
	s.rootID = s.newID()
	s.items[s.rootID] = &item{id: s.rootID, name: "root", dir: true, mtime: time.Now()}
	return s
}

func (s *Server) GraphURL() string { return s.URL + "/v1.0" }
func (s *Server) AuthURL() string  { return s.URL }

// IssueToken returns a valid access token (for tests that skip login).
func (s *Server) IssueToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := randHex()
	s.tokens[t] = true
	return t
}

// RevokeAll invalidates all access tokens (refresh tokens stay valid).
func (s *Server) RevokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

// SetOffline makes every Graph request fail with 503.
func (s *Server) SetOffline(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline = v
}

func (s *Server) RequestCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Requests[method]
}

func randHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) newID() string {
	s.nextID++
	return fmt.Sprintf("ITEM!%d", s.nextID)
}

// lookup resolves a slash-separated path. Caller holds mu.
func (s *Server) lookup(p string) *item {
	cur := s.items[s.rootID]
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		cur = s.child(cur.id, part)
		if cur == nil {
			return nil
		}
	}
	return cur
}

func (s *Server) child(parent, name string) *item {
	for _, it := range s.items {
		if it.parent == parent && strings.EqualFold(it.name, name) {
			return it
		}
	}
	return nil
}

// Put creates or replaces a file at path p, creating parent folders.
func (s *Server) Put(p string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, name := path.Split(strings.Trim(p, "/"))
	parent := s.mkdirAll(dir)
	s.putChild(parent.id, name, data)
}

func (s *Server) Mkdir(p string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirAll(p)
}

func (s *Server) Remove(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.lookup(p)
	if it == nil || it.id == s.rootID {
		return false
	}
	s.deleteTree(it.id)
	return true
}

// Get returns the file content at p.
func (s *Server) Get(p string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := s.lookup(p)
	if it == nil || it.dir {
		return nil, false
	}
	return append([]byte(nil), it.data...), true
}

func (s *Server) Exists(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookup(p) != nil
}

// Paths lists every item path (folders end with "/"), sorted.
func (s *Server) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, it := range s.items {
		if it.id == s.rootID {
			continue
		}
		p := it.name
		for par := s.items[it.parent]; par != nil && par.id != s.rootID; par = s.items[par.parent] {
			p = par.name + "/" + p
		}
		if it.dir {
			p += "/"
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (s *Server) mkdirAll(p string) *item {
	cur := s.items[s.rootID]
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		c := s.child(cur.id, part)
		if c == nil {
			c = &item{id: s.newID(), name: part, parent: cur.id, dir: true, mtime: time.Now()}
			s.items[c.id] = c
		}
		cur = c
	}
	return cur
}

func (s *Server) putChild(parent, name string, data []byte) *item {
	it := s.child(parent, name)
	if it == nil {
		it = &item{id: s.newID(), name: name, parent: parent}
		s.items[it.id] = it
	}
	it.data = append([]byte(nil), data...)
	it.ctag++
	it.mtime = time.Now()
	return it
}

func (s *Server) deleteTree(id string) {
	for cid, c := range s.items {
		if c.parent == id {
			s.deleteTree(cid)
		}
	}
	delete(s.items, id)
}

func (s *Server) json(it *item) map[string]any {
	m := map[string]any{
		"id":                   it.id,
		"name":                 it.name,
		"eTag":                 fmt.Sprintf("\"{%s},%d\"", it.id, it.ctag),
		"cTag":                 fmt.Sprintf("\"c:{%s},%d\"", it.id, it.ctag),
		"lastModifiedDateTime": it.mtime.UTC().Format(time.RFC3339Nano),
		"parentReference":      map[string]string{"id": it.parent},
	}
	if it.dir {
		n := 0
		for _, c := range s.items {
			if c.parent == it.id {
				n++
			}
		}
		m["folder"] = map[string]int{"childCount": n}
		m["size"] = 0
	} else {
		m["file"] = map[string]string{"mimeType": "application/octet-stream"}
		m["size"] = len(it.data)
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func gerr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests[r.Method]++
	p := r.URL.EscapedPath()
	switch {
	case strings.HasSuffix(p, "/oauth2/v2.0/devicecode"):
		writeJSON(w, 200, map[string]any{
			"device_code": "fake-device-code", "user_code": "FAKE-CODE",
			"verification_uri": s.URL + "/devicelogin", "expires_in": 900, "interval": 1,
			"message": "To sign in, use a web browser to open " + s.URL + "/devicelogin and enter the code FAKE-CODE to authenticate.",
		})
		return
	case strings.HasSuffix(p, "/oauth2/v2.0/token"):
		s.serveToken(w, r)
		return
	case strings.HasPrefix(p, "/upload/"):
		s.serveUpload(w, r, strings.TrimPrefix(p, "/upload/"))
		return
	}
	if s.offline {
		gerr(w, 503, "serviceNotAvailable", "offline")
		return
	}
	if !s.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		gerr(w, 401, "InvalidAuthenticationToken", "bad token")
		return
	}
	rest := strings.TrimPrefix(p, "/v1.0")
	switch {
	case rest == "/me/drive":
		used := int64(0)
		for _, it := range s.items {
			used += int64(len(it.data))
		}
		writeJSON(w, 200, map[string]any{"quota": map[string]int64{"total": s.QuotaTotal, "used": used, "remaining": s.QuotaTotal - used}})
	case rest == "/me/drive/root":
		writeJSON(w, 200, s.json(s.items[s.rootID]))
	case strings.HasPrefix(rest, "/me/drive/items/"):
		s.serveItems(w, r, strings.TrimPrefix(rest, "/me/drive/items/"))
	default:
		gerr(w, 404, "itemNotFound", rest)
	}
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	switch r.PostForm.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		if r.PostForm.Get("device_code") != "fake-device-code" {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "bad device code"})
			return
		}
	case "refresh_token":
		if !s.refresh[r.PostForm.Get("refresh_token")] {
			writeJSON(w, 400, map[string]string{"error": "invalid_grant", "error_description": "bad refresh token"})
			return
		}
		delete(s.refresh, r.PostForm.Get("refresh_token"))
	default:
		writeJSON(w, 400, map[string]string{"error": "unsupported_grant_type"})
		return
	}
	at, rt := randHex(), randHex()
	s.tokens[at], s.refresh[rt] = true, true
	writeJSON(w, 200, map[string]any{"access_token": at, "refresh_token": rt, "token_type": "Bearer", "expires_in": s.TokenExpiresIn})
}

func (s *Server) resolve(id string) *item {
	if id == "root" {
		id = s.rootID
	}
	return s.items[id]
}

func (s *Server) serveItems(w http.ResponseWriter, r *http.Request, rest string) {
	var id, name, op string
	if i := strings.Index(rest, ":/"); i >= 0 {
		id = rest[:i]
		tail := rest[i+2:]
		j := strings.Index(tail, ":")
		if j < 0 {
			gerr(w, 400, "invalidRequest", "bad path")
			return
		}
		name, op = tail[:j], strings.TrimPrefix(tail[j+1:], "/")
	} else {
		id, op, _ = strings.Cut(rest, "/")
	}
	id, _ = url.PathUnescape(id)
	name, _ = url.PathUnescape(name)
	it := s.resolve(id)
	if it == nil {
		gerr(w, 404, "itemNotFound", "no item "+id)
		return
	}
	if name != "" {
		c := s.child(it.id, name)
		switch {
		case op == "" && r.Method == http.MethodGet:
			if c == nil {
				gerr(w, 404, "itemNotFound", name)
				return
			}
			writeJSON(w, 200, s.json(c))
		case op == "content" && r.Method == http.MethodPut:
			if c != nil && c.dir {
				gerr(w, 409, "nameAlreadyExists", name)
				return
			}
			data, _ := io.ReadAll(r.Body)
			writeJSON(w, 201, s.json(s.putChild(it.id, name, data)))
		case op == "createUploadSession" && r.Method == http.MethodPost:
			sid := randHex()
			s.sessions[sid] = &session{parent: it.id, name: name}
			writeJSON(w, 200, map[string]any{"uploadUrl": s.URL + "/upload/" + sid})
		default:
			gerr(w, 405, "invalidRequest", r.Method+" "+op)
		}
		return
	}
	switch {
	case op == "" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.json(it))
	case op == "" && r.Method == http.MethodDelete:
		s.deleteTree(it.id)
		w.WriteHeader(204)
	case op == "" && r.Method == http.MethodPatch:
		var in struct {
			Name            string `json:"name"`
			ParentReference *struct {
				ID string `json:"id"`
			} `json:"parentReference"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		parent := s.items[it.parent]
		if in.ParentReference != nil {
			parent = s.resolve(in.ParentReference.ID)
		}
		if parent == nil || !parent.dir {
			gerr(w, 400, "invalidRequest", "bad parent")
			return
		}
		newName := it.name
		if in.Name != "" {
			newName = in.Name
		}
		if c := s.child(parent.id, newName); c != nil && c.id != it.id {
			gerr(w, 409, "nameAlreadyExists", newName)
			return
		}
		it.parent, it.name = parent.id, newName
		writeJSON(w, 200, s.json(it))
	case op == "content" && r.Method == http.MethodGet:
		if it.dir {
			gerr(w, 400, "notSupported", "folder")
			return
		}
		// Real Graph redirects to a pre-authenticated download URL.
		w.Header().Set("Content-Length", strconv.Itoa(len(it.data)))
		_, _ = w.Write(it.data)
	case strings.HasPrefix(op, "children") && r.Method == http.MethodGet:
		s.serveChildren(w, r, it)
	case op == "children" && r.Method == http.MethodPost:
		var in struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if s.child(it.id, in.Name) != nil {
			gerr(w, 409, "nameAlreadyExists", in.Name)
			return
		}
		c := &item{id: s.newID(), name: in.Name, parent: it.id, dir: true, mtime: time.Now()}
		s.items[c.id] = c
		writeJSON(w, 201, s.json(c))
	default:
		gerr(w, 405, "invalidRequest", r.Method+" "+op)
	}
}

func (s *Server) serveChildren(w http.ResponseWriter, r *http.Request, it *item) {
	var kids []*item
	for _, c := range s.items {
		if c.parent == it.id {
			kids = append(kids, c)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].name < kids[j].name })
	skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
	end := min(skip+s.PageSize, len(kids))
	vals := []map[string]any{}
	for _, c := range kids[min(skip, len(kids)):end] {
		vals = append(vals, s.json(c))
	}
	out := map[string]any{"value": vals}
	if end < len(kids) {
		out["@odata.nextLink"] = fmt.Sprintf("%s/v1.0/me/drive/items/%s/children?skip=%d", s.URL, url.PathEscape(it.id), end)
	}
	writeJSON(w, 200, out)
}

func (s *Server) serveUpload(w http.ResponseWriter, r *http.Request, sid string) {
	sess := s.sessions[sid]
	if sess == nil {
		gerr(w, 404, "itemNotFound", "session")
		return
	}
	if r.Method == http.MethodDelete {
		delete(s.sessions, sid)
		w.WriteHeader(204)
		return
	}
	if r.Header.Get("Authorization") != "" {
		gerr(w, 401, "invalidRequest", "upload URL must not carry Authorization")
		return
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(r.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil || start != int64(sess.buf.Len()) {
		gerr(w, 416, "invalidRange", r.Header.Get("Content-Range"))
		return
	}
	if (end-start+1)%(320*1024) != 0 && end+1 != total {
		gerr(w, 400, "invalidRange", "chunk not multiple of 320 KiB")
		return
	}
	n, _ := io.Copy(&sess.buf, r.Body)
	if n != end-start+1 {
		gerr(w, 400, "invalidRange", "short body")
		return
	}
	if int64(sess.buf.Len()) < total {
		writeJSON(w, 202, map[string]any{"nextExpectedRanges": []string{fmt.Sprintf("%d-", sess.buf.Len())}})
		return
	}
	delete(s.sessions, sid)
	writeJSON(w, 201, s.json(s.putChild(sess.parent, sess.name, sess.buf.Bytes())))
}
