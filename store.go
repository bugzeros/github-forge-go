// Store: the forge's whole in-memory state, plus the JSON shapes the REST
// surface returns. Every timestamp derives from a monotonic sequence counter
// (BASE_EPOCH + seq), never the wall clock, so two runs on one instance
// export identical state.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const viewerLogin = "adaiversen"
const viewerID = 1000
const viewerName = "ada iversen"
const viewerEmail = "adaiversen@example.invalid"
const agentName = "github-forge/1.0.0"
const zeroOID = "0000000000000000000000000000000000000000"
const baseEpoch = 1767225600 // 2026-01-01T00:00:00Z, deterministic

// ---------------------------------------------------------------------------
// Git objects
// ---------------------------------------------------------------------------

type objType int

const (
	objCommit objType = 1
	objTree   objType = 2
	objBlob   objType = 3
	objTag    objType = 4
)

type gitObj struct {
	typ  objType
	data []byte
}

func objectID(typ objType, data []byte) string {
	h := sha1.New()
	var hdr [32]byte
	n := copy(hdr[:], typeName(typ))
	n += copy(hdr[n:], " ")
	n += copy(hdr[n:], strconv.Itoa(len(data)))
	hdr[n] = 0
	h.Write(hdr[:n+1])
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func typeName(t objType) string {
	switch t {
	case objCommit:
		return "commit"
	case objTree:
		return "tree"
	case objBlob:
		return "blob"
	default:
		return "tag"
	}
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := indexByte(data[i:], ' ')
		if sp < 0 {
			break
		}
		sp += i
		nul := indexByte(data[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		mode := string(data[i:sp])
		name := string(data[sp+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		i = nul + 21
	}
	return out
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(a, b int) bool {
		ka, kb := sorted[a].name, sorted[b].name
		if sorted[a].mode == "40000" {
			ka += "/"
		}
		if sorted[b].mode == "40000" {
			kb += "/"
		}
		return ka < kb
	})
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.mode+" "+e.name)...)
		out = append(out, 0)
		raw, _ := hex.DecodeString(e.oid)
		out = append(out, raw...)
	}
	return out
}

type commitView struct {
	tree      string
	parents   []string
	message   string
	author    string // full author line
	committer string
	raw       []byte
}

func parseCommit(data []byte) commitView {
	text := string(data)
	headEnd := indexOfStr(text, "\n\n")
	head := text
	msg := ""
	if headEnd >= 0 {
		head = text[:headEnd]
		msg = text[headEnd+2:]
	}
	cv := commitView{raw: data, message: msg}
	for _, line := range splitLines(head) {
		switch {
		case hasPrefix(line, "tree "):
			cv.tree = trimSpace(line[5:])
		case hasPrefix(line, "parent "):
			cv.parents = append(cv.parents, trimSpace(line[7:]))
		case hasPrefix(line, "author "):
			cv.author = line[7:]
		case hasPrefix(line, "committer "):
			cv.committer = line[10:]
		}
	}
	return cv
}

func parseTag(data []byte) (target, typ string) {
	for _, line := range splitLines(string(data)) {
		if hasPrefix(line, "object ") {
			target = trimSpace(line[7:])
		} else if hasPrefix(line, "type ") {
			typ = trimSpace(line[5:])
		}
	}
	return target, typ
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// ---------------------------------------------------------------------------
// Store records
// ---------------------------------------------------------------------------

type comment struct {
	id         int
	body       string
	createdSeq int
}

type issue struct {
	number     int
	id         int
	title      string
	body       string
	state      string // "open" | "closed"
	labels     []string
	createdSeq int
	updatedSeq int
	comments   []*comment
	// Serialized-JSON cache: valid while updatedSeq and comment count match.
	jsonCache     []byte
	jsonCacheSeq  int
	jsonCacheNCom int
}

type pull struct {
	number         int
	id             int
	title          string
	body           string
	state          string // "open" | "closed"
	merged         bool
	mergedAt       string
	mergeCommitSha string
	headRef        string
	headSha        string
	baseRef        string
	baseSha        string
	createdSeq     int
	updatedSeq     int
}

type label struct {
	name  string
	color string
	id    int
}

type repo struct {
	id            int
	name          string
	owner         string
	description   string
	isPrivate     bool
	defaultBranch string
	createdSeq    int
	objects       map[string]*gitObj
	refs          map[string]string
	issues        []*issue
	issuesByNum   map[int]*issue
	pulls         []*pull
	pullsByNum    map[int]*pull
	labels        map[string]*label
	packCache     map[string][]byte
	// fetchCache holds whole v2 fetch responses (ack + shallow-info +
	// wanted-refs + packfile sections) for the cacheable shapes
	// (no haves, no shallow lines, depth nil or 1), keyed like packCache.
	// The 50 concurrent shallow clones all ask for the same bytes.
	fetchCache map[string][]byte
	// cacheMu guards packCache and fetchCache: the clone workload reads
	// them from 50 goroutines at once, and a cold miss writes.
	cacheMu sync.RWMutex
}

func newRepo(id int, name, desc string, priv bool, seq int) *repo {
	return &repo{
		id: id, name: name, owner: viewerLogin, description: desc, isPrivate: priv,
		defaultBranch: "main", createdSeq: seq,
		objects: map[string]*gitObj{}, refs: map[string]string{},
		issuesByNum: map[int]*issue{}, pullsByNum: map[int]*pull{},
		labels: map[string]*label{}, packCache: map[string][]byte{}, fetchCache: map[string][]byte{},
	}
}

func (r *repo) fullName() string { return r.owner + "/" + r.name }
func (r *repo) tip(branch string) string {
	if branch == "" {
		branch = r.defaultBranch
	}
	return r.refs["refs/heads/"+branch]
}
func (r *repo) invalidate() {
	r.cacheMu.Lock()
	r.packCache = map[string][]byte{}
	r.fetchCache = map[string][]byte{}
	r.cacheMu.Unlock()
}

// store is the whole forge state, guarded by one RWMutex. Reads (clone,
// issue GET) run concurrently; writes take the write lock.
type store struct {
	mu sync.RWMutex

	seq          int
	repos        map[string]*repo
	repoIDSeq    int
	issueIDSeq   int
	commentIDSeq int
	labelIDSeq   int
	pullIDSeq    int
	reqSeq       int64
	isoMu        sync.Mutex
	isoCache     map[int]string
}

var db = &store{
	repos:        map[string]*repo{},
	repoIDSeq:    4999,
	issueIDSeq:   99999,
	commentIDSeq: 199999,
	labelIDSeq:   299999,
	pullIDSeq:    89999,
	isoCache:     map[int]string{},
}

func (s *store) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *store) nextReq() int64 {
	return atomic.AddInt64(&s.reqSeq, 1)
}

// isoFor renders BASE_EPOCH+n as an ISO-8601 UTC timestamp without millis.
func (s *store) isoFor(n int) string {
	s.isoMu.Lock()
	defer s.isoMu.Unlock()
	if v, ok := s.isoCache[n]; ok {
		return v
	}
	v := time.Unix(baseEpoch+int64(n), 0).UTC().Format("2006-01-02T15:04:05Z")
	s.isoCache[n] = v
	return v
}

// isoUnix renders an absolute unix timestamp as ISO-8601 UTC without millis.
func (s *store) isoUnix(ts int) string {
	return time.Unix(int64(ts), 0).UTC().Format("2006-01-02T15:04:05Z")
}

func (s *store) getRepo(owner, name string) *repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

// ---------------------------------------------------------------------------
// JSON shapes (mirror the Node entry's field-for-field)
// ---------------------------------------------------------------------------

func userJSON() map[string]any {
	return map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"}
}

func repoJSON(r *repo) map[string]any {
	open := 0
	for _, i := range r.issues {
		if i.state == "open" {
			open++
		}
	}
	return map[string]any{
		"id": r.id, "name": r.name, "full_name": r.fullName(),
		"owner": map[string]any{"login": r.owner, "id": viewerID, "type": "User"},
		"private": r.isPrivate, "description": r.description, "fork": false,
		"html_url": "https://github.com/" + r.fullName(),
		"url":      "https://api.github.com/repos/" + r.fullName(),
		"default_branch": r.defaultBranch, "has_issues": true, "has_projects": true, "has_wiki": true,
		"created_at": db.isoFor(r.createdSeq), "updated_at": db.isoFor(r.createdSeq), "pushed_at": db.isoFor(r.createdSeq),
		"open_issues_count": open,
	}
}

func issueJSON(r *repo, i *issue) map[string]any {
	labels := make([]map[string]any, 0, len(i.labels))
	for _, name := range i.labels {
		color := "ededed"
		if l, ok := r.labels[name]; ok {
			color = l.color
		}
		labels = append(labels, map[string]any{"name": name, "color": color})
	}
	return map[string]any{
		"id": i.id, "number": i.number, "title": i.title, "body": i.body, "state": i.state,
		"labels": labels,
		"user":     map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"assignee": nil, "assignees": []any{}, "comments": len(i.comments),
		"created_at": db.isoFor(i.createdSeq), "updated_at": db.isoFor(i.updatedSeq),
		"html_url": "https://github.com/" + r.fullName() + "/issues/" + strconv.Itoa(i.number),
		"url":      "https://api.github.com/repos/" + r.fullName() + "/issues/" + strconv.Itoa(i.number),
	}
}

// issueJSONBytes returns the cached serialization of one issue.
func issueJSONBytes(r *repo, i *issue) []byte {
	if i.jsonCache != nil && i.jsonCacheSeq == i.updatedSeq && i.jsonCacheNCom == len(i.comments) {
		return i.jsonCache
	}
	b := issueJSONBuf(r, i)
	i.jsonCache = b
	i.jsonCacheSeq = i.updatedSeq
	i.jsonCacheNCom = len(i.comments)
	return b
}

// appendJSONString appends s as a JSON string literal, escaping exactly
// like encoding/json does with HTML escaping on (\" \\ \n \r \t, control
// bytes as \u00XX, < > & as \u003c \u003e \u0026, U+2028/U+2029, invalid
// UTF-8 bytes as the raw U+FFFD bytes), so the bytes match
// json.Marshal's output.
func appendJSONString(b []byte, s string) []byte {
	const hexd = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			switch c {
			case '"':
				b = append(b, '\\', '"')
			case '\\':
				b = append(b, '\\', '\\')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			case '<':
				b = append(b, '\\', 'u', '0', '0', '3', 'c')
			case '>':
				b = append(b, '\\', 'u', '0', '0', '3', 'e')
			case '&':
				b = append(b, '\\', 'u', '0', '0', '2', '6')
			default:
				if c < 0x20 {
					b = append(b, '\\', 'u', '0', '0', hexd[c>>4], hexd[c&0xf])
				} else {
					b = append(b, c)
				}
			}
			i++
			continue
		}
		rn, size := utf8.DecodeRuneInString(s[i:])
		if rn == utf8.RuneError && size == 1 {
			// encoding/json writes the replacement character itself
			// (the raw U+FFFD bytes), not the \ufffd escape.
			b = append(b, 0xef, 0xbf, 0xbd)
			i++
			continue
		}
		if rn == '\u2028' || rn == '\u2029' {
			b = append(b, '\\', 'u', '2', '0', '2', '8')
			if rn == '\u2029' {
				b[len(b)-1] = '9'
			}
			i += size
			continue
		}
		b = append(b, s[i:i+size]...)
		i += size
	}
	return append(b, '"')
}

// issueJSONBuf serializes one issue to exactly the bytes
// json.Marshal(issueJSON(r, i)) produces — same keys in encoding/json's
// sorted order, same escaping — but by appending to a buffer instead of
// building maps and reflecting over them. The write workload (200
// sequential issue POSTs) and the single-issue GET cache both use it.
func issueJSONBuf(r *repo, i *issue) []byte {
	b := make([]byte, 0, 512)
	b = append(b, `{"assignee":null,"assignees":[],"body":`...)
	b = appendJSONString(b, i.body)
	b = append(b, `,"comments":`...)
	b = strconv.AppendInt(b, int64(len(i.comments)), 10)
	b = append(b, `,"created_at":`...)
	b = appendJSONString(b, db.isoFor(i.createdSeq))
	b = append(b, `,"html_url":`...)
	b = appendJSONString(b, "https://github.com/"+r.fullName()+"/issues/"+strconv.Itoa(i.number))
	b = append(b, `,"id":`...)
	b = strconv.AppendInt(b, int64(i.id), 10)
	b = append(b, `,"labels":[`...)
	for j, name := range i.labels {
		if j > 0 {
			b = append(b, ',')
		}
		color := "ededed"
		if l, ok := r.labels[name]; ok {
			color = l.color
		}
		b = append(b, `{"color":`...)
		b = appendJSONString(b, color)
		b = append(b, `,"name":`...)
		b = appendJSONString(b, name)
		b = append(b, '}')
	}
	b = append(b, `],"number":`...)
	b = strconv.AppendInt(b, int64(i.number), 10)
	b = append(b, `,"state":`...)
	b = appendJSONString(b, i.state)
	b = append(b, `,"title":`...)
	b = appendJSONString(b, i.title)
	b = append(b, `,"updated_at":`...)
	b = appendJSONString(b, db.isoFor(i.updatedSeq))
	b = append(b, `,"url":`...)
	b = appendJSONString(b, "https://api.github.com/repos/"+r.fullName()+"/issues/"+strconv.Itoa(i.number))
	b = append(b, `,"user":{"id":`...)
	b = strconv.AppendInt(b, int64(viewerID), 10)
	b = append(b, `,"login":`...)
	b = appendJSONString(b, viewerLogin)
	b = append(b, `,"type":"User"}}`...)
	return b
}

func pullJSON(r *repo, p *pull) map[string]any {
	return map[string]any{
		"id": p.id, "number": p.number, "title": p.title, "body": p.body, "state": p.state,
		"merged": p.merged, "mergeable": true, "mergeable_state": "clean",
		"merged_at": p.mergedAt, "merge_commit_sha": p.mergeCommitSha,
		"head": map[string]any{"ref": p.headRef, "sha": p.headSha, "label": viewerLogin + ":" + p.headRef},
		"base": map[string]any{"ref": p.baseRef, "sha": p.baseSha, "label": viewerLogin + ":" + p.baseRef},
		"user": map[string]any{"login": viewerLogin, "id": viewerID, "type": "User"},
		"created_at": db.isoFor(p.createdSeq), "updated_at": db.isoFor(p.updatedSeq),
		"html_url": "https://github.com/" + r.fullName() + "/pull/" + strconv.Itoa(p.number),
		"url":      "https://api.github.com/repos/" + r.fullName() + "/pulls/" + strconv.Itoa(p.number),
	}
}

func gid(kind string, parts ...any) string {
	s := kind
	for _, p := range parts {
		s += ":"
		switch v := p.(type) {
		case string:
			s += v
		case int:
			s += strconv.Itoa(v)
		}
	}
	return b64encode([]byte(s))
}

func b64encode(b []byte) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		rem := len(b) - i
		n = uint32(b[i]) << 16
		if rem > 1 {
			n |= uint32(b[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(b[i+2])
		}
		out = append(out, alpha[(n>>18)&63], alpha[(n>>12)&63])
		if rem > 1 {
			out = append(out, alpha[(n>>6)&63])
		} else {
			out = append(out, '=')
		}
		if rem > 2 {
			out = append(out, alpha[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

func b64decode(s string) ([]byte, bool) {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	rev := [256]int{}
	for i := range rev {
		rev[i] = -1
	}
	for i := 0; i < len(alpha); i++ {
		rev[alpha[i]] = i
	}
	var out []byte
	var acc uint32
	var nbits int
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '=' {
			break
		}
		v := rev[c]
		if v < 0 {
			return nil, false
		}
		acc = (acc << 6) | uint32(v)
		nbits += 6
		if nbits >= 8 {
			nbits -= 8
			out = append(out, byte(acc>>uint(nbits)))
		}
	}
	return out, true
}

func parseGid(id string) (kind string, parts []string, ok bool) {
	raw, good := b64decode(id)
	if !good {
		return "", nil, false
	}
	s := string(raw)
	segs := splitStr(s, ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

func splitStr(s, sep string) []string {
	var out []string
	start := 0
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			out = append(out, s[start:i])
			start = i + len(sep)
		}
	}
	out = append(out, s[start:])
	return out
}

// flattenTree collects path -> {mode, oid} for every blob in a tree.
func flattenTree(r *repo, treeOID, prefix string, out map[string]treeEntry) {
	obj, ok := r.objects[treeOID]
	if !ok || obj.typ != objTree {
		return
	}
	for _, e := range parseTree(obj.data) {
		path := e.name
		if prefix != "" {
			path = prefix + "/" + e.name
		}
		if e.mode == "40000" {
			flattenTree(r, e.oid, path, out)
		} else {
			out[path] = e
		}
	}
}

func buildTreeFromFlat(r *repo, flat map[string]treeEntry) string {
	type dirNode struct {
		files map[string]treeEntry
		dirs  map[string]*dirNode
	}
	root := &dirNode{files: map[string]treeEntry{}, dirs: map[string]*dirNode{}}
	for path, ent := range flat {
		parts := splitStr(path, "/")
		node := root
		for i := 0; i < len(parts)-1; i++ {
			child, ok := node.dirs[parts[i]]
			if !ok {
				child = &dirNode{files: map[string]treeEntry{}, dirs: map[string]*dirNode{}}
				node.dirs[parts[i]] = child
			}
			node = child
		}
		node.files[parts[len(parts)-1]] = ent
	}
	var build func(n *dirNode) string
	build = func(n *dirNode) string {
		var entries []treeEntry
		for name, ent := range n.files {
			entries = append(entries, treeEntry{mode: ent.mode, name: name, oid: ent.oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
		}
		data := buildTree(entries)
		oid := objectID(objTree, data)
		r.objects[oid] = &gitObj{typ: objTree, data: data}
		return oid
	}
	return build(root)
}

func commitWalk(r *repo, start string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		out = append(out, oid)
		cv := parseCommit(obj.data)
		queue = append(queue, cv.parents...)
	}
	return out
}

func isAncestor(r *repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		queue = append(queue, parseCommit(obj.data).parents...)
	}
	return false
}

func findMergeBase(r *repo, a, b string) string {
	ancA := map[string]bool{}
	for _, c := range commitWalk(r, a, 10000) {
		ancA[c] = true
	}
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancA[oid] {
			return oid
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		queue = append(queue, parseCommit(obj.data).parents...)
	}
	return ""
}
