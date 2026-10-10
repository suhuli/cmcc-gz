// Package cloudtest 提供内存版的中国移动云盘服务端，供各层测试使用。
//
// 它实现了客户端用到的全部接口：列目录、建目录、改名、移动、回收站、
// 下载地址、分片上传、短信登录，以及按 Range 下载。
package cloudtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcloudmount/internal/cloud"
)

// Node 是云端的一个文件或目录。
type Node struct {
	ID      string
	Parent  string
	Name    string
	IsDir   bool
	Data    []byte
	Updated time.Time
	Trashed bool
}

type upload struct {
	node     *Node
	parts    map[int][]byte
	expected []int64
	hash     string
}

// Server 是内存云盘。
type Server struct {
	mu      sync.Mutex
	HTTP    *httptest.Server
	nodes   map[string]*Node
	uploads map[string]*upload
	seq     int

	// Token 是被接受的令牌。
	Token string
	// PageSize 覆盖客户端请求的分页大小（测试翻页）。
	PageSize int
	// FailNext 让指定路径的下 N 次请求返回 503。
	FailNext map[string]int
	// Calls 统计每个接口的调用次数。
	Calls map[string]int
	// SMSCode 是登录接受的验证码。
	SMSCode string
	// RangeIgnored 让下载忽略 Range（返回 200 全量）。
	RangeIgnored bool
	// PartURLsInCreate 为 false 时 create 不返回上传地址，强制客户端调用 getUploadUrl。
	PartURLsInCreate bool
	// ListDelay 模拟列目录延迟。
	ListDelay time.Duration
}

// New 启动内存云盘。
func New() *Server {
	s := &Server{
		nodes:            map[string]*Node{"/": {ID: "/", Name: "", IsDir: true}},
		uploads:          map[string]*upload{},
		Token:            "tok",
		FailNext:         map[string]int{},
		Calls:            map[string]int{},
		SMSCode:          "123456",
		PartURLsInCreate: true,
	}
	s.HTTP = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Close 关闭服务。
func (s *Server) Close() { s.HTTP.Close() }

// PersonalURL 是个人云接口地址。
func (s *Server) PersonalURL() string { return s.HTTP.URL + "/hcy/" }

// UserURL 是登录服务地址。
func (s *Server) UserURL() string { return s.HTTP.URL + "/user" }

func (s *Server) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s%04d", prefix, s.seq)
}

// AddDir 直接创建目录（测试准备数据用）。
func (s *Server) AddDir(parent, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("d")
	s.nodes[id] = &Node{ID: id, Parent: parent, Name: name, IsDir: true, Updated: time.Now()}
	return id
}

// AddFile 直接创建文件。
func (s *Server) AddFile(parent, name string, data []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID("f")
	s.nodes[id] = &Node{ID: id, Parent: parent, Name: name, Data: append([]byte(nil), data...), Updated: time.Now()}
	return id
}

// Find 按路径查找（区分大小写），不存在返回 nil。
func (s *Server) Find(p string) *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.nodes["/"]
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == "" {
			continue
		}
		var next *Node
		for _, n := range s.nodes {
			if n.Parent == cur.ID && n.Name == part && !n.Trashed {
				next = n
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	cp := *cur
	cp.Data = append([]byte(nil), cur.Data...)
	return &cp
}

// Children 返回目录下未删除的名字（排序）。
func (s *Server) Children(parent string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, n := range s.nodes {
		if n.Parent == parent && !n.Trashed && n.ID != "/" {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

// CallCount 返回接口调用次数。
func (s *Server) CallCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Calls[name]
}

func (s *Server) childByName(parent, name string) *Node {
	for _, n := range s.nodes {
		if n.Parent == parent && n.Name == name && !n.Trashed && n.ID != "/" {
			return n
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func ok(data any) map[string]any { return map[string]any{"code": "0", "success": true, "data": data} }

func fail(code, msg string) map[string]any {
	return map[string]any{"code": code, "success": false, "message": msg}
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	name := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case strings.HasPrefix(name, "upload/"):
		name = "upload"
	case strings.HasPrefix(name, "dl/"):
		name = "download"
	}
	s.Calls[name]++
	if s.FailNext[name] > 0 {
		s.FailNext[name]--
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	s.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/hcy/"):
		s.servePersonal(w, r, strings.TrimPrefix(r.URL.Path, "/hcy/"))
	case strings.HasPrefix(r.URL.Path, "/user/"):
		s.serveUser(w, r, strings.TrimPrefix(r.URL.Path, "/user/"))
	case strings.HasPrefix(r.URL.Path, "/upload/"):
		s.servePart(w, r)
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		s.serveDownload(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(raw), ":", 3)
	return len(parts) == 3 && parts[2] == s.Token && r.Header.Get("mcloud-sign") != ""
}

func (s *Server) servePersonal(w http.ResponseWriter, r *http.Request, op string) {
	body, _ := io.ReadAll(r.Body)
	if !s.authorized(r) {
		writeJSON(w, fail("200000401", "token invalid"))
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, fail("400", "bad json"))
		return
	}
	if op == "file/list" && s.ListDelay > 0 {
		time.Sleep(s.ListDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch op {
	case "file/list":
		s.opList(w, req)
	case "file/create":
		s.opCreate(w, r, req)
	case "file/update":
		id, _ := req["fileId"].(string)
		name, _ := req["name"].(string)
		n := s.nodes[id]
		if n == nil || n.Trashed {
			writeJSON(w, fail("200000404", "not found"))
			return
		}
		if other := s.childByName(n.Parent, name); other != nil && other != n {
			writeJSON(w, fail("200000409", "name exists"))
			return
		}
		n.Name = name
		n.Updated = time.Now()
		writeJSON(w, ok(map[string]any{"fileId": id, "name": name}))
	case "file/batchMove":
		ids, _ := req["fileIds"].([]any)
		to, _ := req["toParentFileId"].(string)
		if dst := s.nodes[to]; dst == nil || !dst.IsDir {
			writeJSON(w, fail("200000404", "parent not found"))
			return
		}
		for _, v := range ids {
			n := s.nodes[v.(string)]
			if n == nil {
				writeJSON(w, fail("200000404", "not found"))
				return
			}
			if other := s.childByName(to, n.Name); other != nil && other != n {
				writeJSON(w, fail("200000409", "name exists"))
				return
			}
		}
		for _, v := range ids {
			s.nodes[v.(string)].Parent = to
		}
		writeJSON(w, ok(map[string]any{}))
	case "recyclebin/batchTrash":
		ids, _ := req["fileIds"].([]any)
		for _, v := range ids {
			if n := s.nodes[v.(string)]; n != nil {
				s.trash(n)
			}
		}
		writeJSON(w, ok(map[string]any{}))
	case "file/getDownloadUrl":
		id, _ := req["fileId"].(string)
		n := s.nodes[id]
		if n == nil || n.Trashed || n.IsDir {
			writeJSON(w, fail("200000404", "not found"))
			return
		}
		writeJSON(w, ok(map[string]any{"url": s.HTTP.URL + "/dl/" + id}))
	case "file/getUploadUrl":
		uid, _ := req["uploadId"].(string)
		up := s.uploads[uid]
		if up == nil {
			writeJSON(w, fail("200000404", "upload not found"))
			return
		}
		parts, _ := req["partInfos"].([]any)
		var out []map[string]any
		for _, p := range parts {
			pm := p.(map[string]any)
			num := int(pm["partNumber"].(float64))
			out = append(out, map[string]any{"partNumber": num, "uploadUrl": fmt.Sprintf("%s/upload/%s/%d", s.HTTP.URL, uid, num)})
		}
		writeJSON(w, ok(map[string]any{"partInfos": out, "transferToken": "tt"}))
	case "file/complete":
		uid, _ := req["uploadId"].(string)
		up := s.uploads[uid]
		if up == nil {
			writeJSON(w, fail("200000404", "upload not found"))
			return
		}
		var buf bytes.Buffer
		for i, size := range up.expected {
			data, ok := up.parts[i+1]
			if !ok || int64(len(data)) != size {
				writeJSON(w, fail("400", fmt.Sprintf("part %d missing or wrong size", i+1)))
				return
			}
			buf.Write(data)
		}
		sum := sha256.Sum256(buf.Bytes())
		if hex.EncodeToString(sum[:]) != strings.ToLower(up.hash) {
			writeJSON(w, fail("400", "hash mismatch"))
			return
		}
		up.node.Data = buf.Bytes()
		up.node.Updated = time.Now()
		s.nodes[up.node.ID] = up.node
		delete(s.uploads, uid)
		writeJSON(w, ok(map[string]any{"fileId": up.node.ID}))
	default:
		writeJSON(w, fail("404", "unknown op "+op))
	}
}

func (s *Server) trash(n *Node) {
	n.Trashed = true
	for _, c := range s.nodes {
		if c.Parent == n.ID && !c.Trashed {
			s.trash(c)
		}
	}
}

func (s *Server) opList(w http.ResponseWriter, req map[string]any) {
	parent, _ := req["parentFileId"].(string)
	if p := s.nodes[parent]; p == nil || p.Trashed || !p.IsDir {
		writeJSON(w, fail("200000404", "parent not found"))
		return
	}
	pi, _ := req["pageInfo"].(map[string]any)
	size := 200
	if v, ok := pi["pageSize"].(float64); ok {
		size = int(v)
	}
	if s.PageSize > 0 {
		size = s.PageSize
	}
	start := 0
	if c, _ := pi["pageCursor"].(string); c != "" {
		start, _ = strconv.Atoi(c)
	}
	var kids []*Node
	for _, n := range s.nodes {
		if n.Parent == parent && !n.Trashed && n.ID != "/" {
			kids = append(kids, n)
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name < kids[j].Name })
	end := start + size
	next := strconv.Itoa(end)
	if end >= len(kids) {
		end = len(kids)
		next = ""
	}
	var items []map[string]any
	for _, n := range kids[start:end] {
		typ := "file"
		if n.IsDir {
			typ = "folder"
		}
		sum := sha256.Sum256(n.Data)
		items = append(items, map[string]any{
			"fileId": n.ID, "parentFileId": n.Parent, "name": n.Name, "type": typ,
			"size": len(n.Data), "updatedAt": n.Updated.Format("2006-01-02T15:04:05.000-07:00"),
			"contentHash": hex.EncodeToString(sum[:]),
		})
	}
	writeJSON(w, ok(map[string]any{"items": items, "nextPageCursor": next}))
}

func uniqueName(s *Server, parent, name string) string {
	if s.childByName(parent, name) == nil {
		return name
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
		if s.childByName(parent, cand) == nil {
			return cand
		}
	}
}

func (s *Server) opCreate(w http.ResponseWriter, r *http.Request, req map[string]any) {
	parent, _ := req["parentFileId"].(string)
	name, _ := req["name"].(string)
	typ, _ := req["type"].(string)
	if p := s.nodes[parent]; p == nil || !p.IsDir || p.Trashed {
		writeJSON(w, fail("200000404", "parent not found"))
		return
	}
	if typ == "folder" {
		if s.childByName(parent, name) != nil {
			writeJSON(w, fail("200000409", "exists"))
			return
		}
		id := s.nextID("d")
		s.nodes[id] = &Node{ID: id, Parent: parent, Name: name, IsDir: true, Updated: time.Now()}
		writeJSON(w, ok(map[string]any{"fileId": id, "fileName": name, "parentFileId": parent}))
		return
	}
	mode, _ := req["fileRenameMode"].(string)
	if s.childByName(parent, name) != nil {
		if mode == "refuse" {
			writeJSON(w, fail("200000409", "exists"))
			return
		}
		name = uniqueName(s, parent, name)
	}
	size := int64(req["size"].(float64))
	hash, _ := req["contentHash"].(string)
	id := s.nextID("f")
	node := &Node{ID: id, Parent: parent, Name: name, Updated: time.Now()}
	// 秒传：已有相同内容的文件
	if size > 0 {
		for _, n := range s.nodes {
			sum := sha256.Sum256(n.Data)
			if !n.IsDir && !n.Trashed && int64(len(n.Data)) == size && hex.EncodeToString(sum[:]) == hash {
				node.Data = append([]byte(nil), n.Data...)
				s.nodes[id] = node
				writeJSON(w, ok(map[string]any{"fileId": id, "fileName": name, "rapidUpload": true}))
				return
			}
		}
	}
	uid := s.nextID("u")
	up := &upload{node: node, parts: map[int][]byte{}, hash: hash}
	parts, _ := req["partInfos"].([]any)
	var out []map[string]any
	ps := cloud.PartSize(size)
	n := int((size + ps - 1) / ps)
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		sz := ps
		if int64(i+1)*ps > size {
			sz = size - int64(i)*ps
		}
		up.expected = append(up.expected, sz)
	}
	if s.PartURLsInCreate {
		for _, p := range parts {
			pm := p.(map[string]any)
			num := int(pm["partNumber"].(float64))
			out = append(out, map[string]any{"partNumber": num, "uploadUrl": fmt.Sprintf("%s/upload/%s/%d", s.HTTP.URL, uid, num)})
		}
	}
	s.uploads[uid] = up
	writeJSON(w, ok(map[string]any{"fileId": id, "fileName": name, "uploadId": uid, "partInfos": out}))
}

func (s *Server) servePart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	segs := strings.Split(strings.TrimPrefix(r.URL.Path, "/upload/"), "/")
	if len(segs) != 2 {
		http.NotFound(w, r)
		return
	}
	num, _ := strconv.Atoi(segs[1])
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	up := s.uploads[segs[0]]
	if up == nil {
		http.NotFound(w, r)
		return
	}
	up.parts[num] = data
	w.WriteHeader(http.StatusOK)
}

func (s *Server) serveDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/dl/")
	s.mu.Lock()
	n := s.nodes[id]
	var data []byte
	if n != nil && !n.Trashed {
		data = append([]byte(nil), n.Data...)
	}
	ignore := s.RangeIgnored
	s.mu.Unlock()
	if n == nil || n.Trashed {
		http.NotFound(w, r)
		return
	}
	rng := r.Header.Get("Range")
	if rng == "" || ignore {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
		return
	}
	var start int
	fmt.Sscanf(rng, "bytes=%d-", &start)
	if start >= len(data) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(data)-1, len(data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)-start))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(data[start:])
}

func (s *Server) serveUser(w http.ResponseWriter, r *http.Request, op string) {
	body, _ := io.ReadAll(r.Body)
	clear, err := cloud.DecryptTransport(string(body))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var req map[string]any
	_ = json.Unmarshal([]byte(clear), &req)
	reply := func(v any) {
		raw, _ := json.Marshal(v)
		enc, _ := cloud.EncryptTransport(string(raw), nil)
		_, _ = io.WriteString(w, enc)
	}
	switch op {
	case "sms/getSmsCode":
		reply(map[string]any{"code": "0", "success": true})
	case "thirdlogin":
		if req["dycpwd"] != s.SMSCode {
			reply(map[string]any{"code": "200059505", "success": false, "message": "验证码错误"})
			return
		}
		data := map[string]any{
			"return": "0", "authToken": s.Token, "token": "refresh-" + s.Token,
			"userid": "u1", "account": req["msisdn"], "atExpiretime": time.Now().Add(30 * 24 * time.Hour).UnixMilli(),
		}
		reply(map[string]any{"code": "0", "success": true, "data": data})
	default:
		http.NotFound(w, r)
	}
}
