package davfs

import (
	"encoding/xml"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/webdav"
)

type (
	xmlName   = xml.Name
	Property  = webdav.Property
	Proppatch = webdav.Proppatch
	Propstat  = webdav.Propstat
)

// maxPropPaths 限制内存中保存属性的路径数量。
const maxPropPaths = 20000

// propStore 在内存中保存客户端设置的自定义属性（PROPPATCH）。
// Windows 复制文件后会设置 Win32LastModifiedTime 等属性，没有存储时会报错。
type propStore struct {
	mu sync.Mutex
	m  map[string]map[xml.Name]webdav.Property
}

func newPropStore() *propStore { return &propStore{m: map[string]map[xml.Name]webdav.Property{}} }

func (s *propStore) get(k string) map[xml.Name]webdav.Property {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.m[k]
	if len(src) == 0 {
		return nil
	}
	out := make(map[xml.Name]webdav.Property, len(src))
	for n, p := range src {
		out[n] = p
	}
	return out
}

func (s *propStore) patch(k string, patches []webdav.Proppatch) []webdav.Propstat {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.m[k]
	if cur == nil {
		if len(s.m) >= maxPropPaths {
			s.m = map[string]map[xml.Name]webdav.Property{}
		}
		cur = map[xml.Name]webdav.Property{}
		s.m[k] = cur
	}
	stat := webdav.Propstat{Status: http.StatusOK}
	for _, p := range patches {
		for _, prop := range p.Props {
			if p.Remove {
				delete(cur, prop.XMLName)
			} else {
				cur[prop.XMLName] = prop
			}
			stat.Props = append(stat.Props, webdav.Property{XMLName: prop.XMLName})
		}
	}
	if len(cur) == 0 {
		delete(s.m, k)
	}
	return []webdav.Propstat{stat}
}

func (s *propStore) move(from, to string) {
	if from == to {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	moved := map[string]map[xml.Name]webdav.Property{}
	for k, v := range s.m {
		if k == from || strings.HasPrefix(k, from+"/") {
			moved[to+strings.TrimPrefix(k, from)] = v
			delete(s.m, k)
		}
	}
	for k, v := range moved {
		s.m[k] = v
	}
}

func (s *propStore) removeTree(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for x := range s.m {
		if x == k || strings.HasPrefix(x, k+"/") {
			delete(s.m, x)
		}
	}
}
