// Git: pkt-line framing, packfiles (read/write, OFS/REF deltas), fetch
// planning, receive-pack and upload-pack (v0 + v2). Ported behavior-for-
// behavior from the Node entry, which passes all 30 git checks.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	var hdr [4]byte
	hex4(hdr[:], n)
	out := make([]byte, 0, n)
	out = append(out, hdr[:]...)
	return append(out, payload...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

func hex4(dst []byte, n int) {
	const digits = "0123456789abcdef"
	dst[0] = digits[(n>>12)&0xf]
	dst[1] = digits[(n>>8)&0xf]
	dst[2] = digits[(n>>4)&0xf]
	dst[3] = digits[n&0xf]
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pkt struct {
	kind    byte // 'd' data, 'f' flush, 'l' delim
	payload []byte
}

func decodePktLines(buf []byte) []pkt {
	var out []pkt
	i := 0
	for i+4 <= len(buf) {
		n, err := strconv.ParseUint(string(buf[i:i+4]), 16, 32)
		if err != nil {
			break
		}
		if n == 0 {
			out = append(out, pkt{kind: 'f'})
			i += 4
			continue
		}
		if n == 1 {
			out = append(out, pkt{kind: 'l'})
			i += 4
			continue
		}
		if i+int(n) > len(buf) {
			break
		}
		out = append(out, pkt{kind: 'd', payload: buf[i+4 : i+int(n)]})
		i += int(n)
	}
	return out
}

func sidebandWrap(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); i += 60000 {
		end := i + 60000
		if end > len(data) {
			end = len(data)
		}
		chunk := make([]byte, 0, end-i+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, data[i:end]...)
		out = append(out, pktLine(chunk)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

// inflateOne inflates one zlib stream starting at start and reports the
// input bytes consumed. Go's flate reads byte-by-byte from a *bytes.Reader,
// so the position after ReadAll is exact (verified empirically).
func inflateOne(buf []byte, start int) ([]byte, int, error) {
	r := bytes.NewReader(buf[start:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, err
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := len(buf) - start - r.Len()
	return data, start + consumed, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	p := 0
	readV := func() (int, error) {
		v := 0
		shift := 0
		for {
			if p >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[p]
			p++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readV()
	if err != nil {
		return nil, err
	}
	dstSize, err := readV()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	var out []byte
	for p < len(delta) {
		op := delta[p]
		p++
		if op&0x80 != 0 {
			off, size := 0, 0
			if op&0x01 != 0 {
				off = int(delta[p])
				p++
			}
			if op&0x02 != 0 {
				off |= int(delta[p]) << 8
				p++
			}
			if op&0x04 != 0 {
				off |= int(delta[p]) << 16
				p++
			}
			if op&0x08 != 0 {
				off |= int(delta[p]) << 24
				p++
			}
			if op&0x10 != 0 {
				size = int(delta[p])
				p++
			}
			if op&0x20 != 0 {
				size |= int(delta[p]) << 8
				p++
			}
			if op&0x40 != 0 {
				size |= int(delta[p]) << 16
				p++
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op != 0 {
			if p+int(op) > len(delta) {
				return nil, errors.New("delta insert out of range")
			}
			out = append(out, delta[p:p+int(op)]...)
			p += int(op)
		} else {
			return nil, errors.New("delta op 0")
		}
	}
	if len(out) != dstSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

var packTypeByID = map[int]objType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}
var packIDByType = map[objType]int{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}

// readPack parses a packfile into objects (oid -> object). Deltas resolve
// against earlier objects in the same pack or objects already in the repo.
func readPack(buf []byte, r *repo) (map[string]*gitObj, error) {
	if len(buf) < 12 || string(buf[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	count := int(binary.BigEndian.Uint32(buf[8:12]))
	pos := 12
	byOffset := map[int]*gitObj{}
	result := map[string]*gitObj{}
	for n := 0; n < count; n++ {
		start := pos
		b := buf[pos]
		pos++
		typeID := int(b>>4) & 0x07
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			b = buf[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		baseOff := -1
		baseOID := ""
		if typeID == 6 {
			// OFS_DELTA: negative offset varint.
			b = buf[pos]
			pos++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buf[pos]
				pos++
				v = ((v + 1) << 7) | int(b&0x7f)
			}
			baseOff = start - v
		} else if typeID == 7 {
			baseOID = hex.EncodeToString(buf[pos : pos+20])
			pos += 20
		} else if _, ok := packTypeByID[typeID]; !ok {
			return nil, errors.New("unknown pack type")
		}
		data, next, err := inflateOne(buf, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		if typeID == 6 || typeID == 7 {
			var base *gitObj
			if typeID == 6 {
				base = byOffset[baseOff]
			} else {
				base = result[baseOID]
				if base == nil {
					base = r.objects[baseOID]
				}
			}
			if base == nil {
				return nil, errors.New("delta base not found")
			}
			applied, err := applyDelta(base.data, data)
			if err != nil {
				return nil, err
			}
			obj := &gitObj{typ: base.typ, data: applied}
			oid := objectID(obj.typ, obj.data)
			byOffset[start] = obj
			result[oid] = obj
		} else {
			typ := packTypeByID[typeID]
			if len(data) != size {
				return nil, errors.New("pack object size mismatch")
			}
			obj := &gitObj{typ: typ, data: data}
			oid := objectID(typ, data)
			byOffset[start] = obj
			result[oid] = obj
		}
	}
	return result, nil
}

func writePack(objects []*gitObj) []byte {
	var body bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objects)))
	body.Write(hdr)
	for _, obj := range objects {
		typeID := packIDByType[obj.typ]
		size := len(obj.data)
		var hb []byte
		first := byte((typeID << 4) | (size & 0x0f))
		size >>= 4
		if size != 0 {
			first |= 0x80
		}
		hb = append(hb, first)
		for size != 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size != 0 {
				b |= 0x80
			}
			hb = append(hb, b)
		}
		body.Write(hb)
		zw, _ := zlib.NewWriterLevel(&body, 6)
		zw.Write(obj.data)
		zw.Close()
	}
	out := body.Bytes()
	sum := sha1sum(out)
	return append(out, sum[:]...)
}

func sha1sum(b []byte) [20]byte {
	return sha1.Sum(b)
}

// ---------------------------------------------------------------------------
// Fetch planning (v2 + v0 share this)
// ---------------------------------------------------------------------------

type fetchPlan struct {
	pack       []byte
	shallow    []string
	unshallow  []string
	wantedRefs []struct{ oid, ref string }
}

func peelToCommit(r *repo, oid string) string {
	cur := oid
	for i := 0; i < 8; i++ {
		obj, ok := r.objects[cur]
		if !ok {
			return ""
		}
		if obj.typ == objCommit {
			return cur
		}
		if obj.typ == objTag {
			target, _ := parseTag(obj.data)
			cur = target
			continue
		}
		return ""
	}
	return ""
}

func computeFetch(r *repo, wants, haves, shallowIn []string, depth *int) fetchPlan {
	haveSet := map[string]bool{}
	for _, h := range haves {
		haveSet[h] = true
	}
	shallowSet := map[string]bool{}
	for _, s := range shallowIn {
		shallowSet[s] = true
	}
	var wantedRefs []struct{ oid, ref string }
	// Sort refs for deterministic output.
	refNames := make([]string, 0, len(r.refs))
	for name := range r.refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)
	for _, name := range refNames {
		oid := r.refs[name]
		for _, w := range wants {
			if oid == w {
				wantedRefs = append(wantedRefs, struct{ oid, ref string }{w, name})
			}
		}
	}
	// BFS over commits from wants.
	gen := map[string]int{}
	var queue []string
	for _, w := range wants {
		c := peelToCommit(r, w)
		if c != "" {
			if _, ok := gen[c]; !ok {
				gen[c] = 0
				queue = append(queue, c)
			}
		}
	}
	selected := map[string]bool{}
	visited := map[string]bool{}
	boundary := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		g := gen[oid]
		if depth != nil && g >= *depth {
			continue
		}
		visited[oid] = true
		if shallowSet[oid] && g > 0 {
			continue
		}
		selected[oid] = true
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		parents := parseCommit(obj.data).parents
		if depth != nil && g == *depth-1 {
			if len(parents) > 0 {
				boundary[oid] = true
			}
			continue
		}
		if depth == nil && shallowSet[oid] && g > 0 {
			continue
		}
		for _, p := range parents {
			if _, ok := gen[p]; !ok {
				gen[p] = g + 1
				queue = append(queue, p)
			}
		}
	}
	var shallow, unshallow []string
	if depth != nil {
		for b := range boundary {
			shallow = append(shallow, b)
		}
		for _, s := range shallowIn {
			obj, ok := r.objects[s]
			if !ok || obj.typ != objCommit {
				continue
			}
			if !selected[s] {
				continue
			}
			parents := parseCommit(obj.data).parents
			all := true
			for _, p := range parents {
				if !selected[p] {
					all = false
				}
			}
			if all {
				unshallow = append(unshallow, s)
			}
		}
	} else if len(shallowIn) > 0 {
		for _, s := range shallowIn {
			if !visited[s] {
				continue
			}
			obj, ok := r.objects[s]
			if !ok || obj.typ != objCommit {
				continue
			}
			parents := parseCommit(obj.data).parents
			if len(parents) > 0 {
				all := true
				for _, p := range parents {
					if !selected[p] {
						all = false
					}
				}
				if all {
					unshallow = append(unshallow, s)
				} else {
					shallow = append(shallow, s)
				}
			} else {
				shallow = append(shallow, s)
			}
		}
	}
	// Objects reachable from a have-commit are already held (plain fetch only).
	held := map[string]bool{}
	if depth == nil {
		for _, h := range haves {
			hc := peelToCommit(r, h)
			if hc == "" {
				continue
			}
			for _, c := range commitWalk(r, hc, 100000) {
				held[c] = true
				co, ok := r.objects[c]
				if !ok || co.typ != objCommit {
					continue
				}
				var addTree func(treeOID string)
				addTree = func(treeOID string) {
					if held[treeOID] {
						return
					}
					held[treeOID] = true
					t, ok := r.objects[treeOID]
					if !ok || t.typ != objTree {
						return
					}
					for _, e := range parseTree(t.data) {
						if e.mode == "40000" {
							addTree(e.oid)
						} else {
							held[e.oid] = true
						}
					}
				}
				addTree(parseCommit(co.data).tree)
			}
		}
	}
	include := map[string]*gitObj{}
	var addObj func(oid string)
	addObj = func(oid string) {
		if _, ok := include[oid]; ok || held[oid] {
			return
		}
		obj, ok := r.objects[oid]
		if !ok {
			return
		}
		include[oid] = obj
	}
	for oid := range selected {
		if haveSet[oid] {
			continue
		}
		addObj(oid)
		obj, ok := r.objects[oid]
		if !ok || obj.typ != objCommit {
			continue
		}
		var collectTree func(treeOID string)
		collectTree = func(treeOID string) {
			addObj(treeOID)
			t, ok := r.objects[treeOID]
			if !ok || t.typ != objTree {
				return
			}
			for _, e := range parseTree(t.data) {
				if e.mode == "40000" {
					collectTree(e.oid)
				} else {
					addObj(e.oid)
				}
			}
		}
		collectTree(parseCommit(obj.data).tree)
	}
	for _, w := range wants {
		if obj, ok := r.objects[w]; ok && obj.typ == objTag {
			addObj(w)
		}
	}
	// Deterministic pack order: sort object ids.
	oids := make([]string, 0, len(include))
	for oid := range include {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objs := make([]*gitObj, 0, len(oids))
	for _, oid := range oids {
		objs = append(objs, include[oid])
	}
	return fetchPlan{pack: writePack(objs), shallow: shallow, unshallow: unshallow, wantedRefs: wantedRefs}
}

func cachedFetch(r *repo, wants, haves, shallowIn []string, depth *int) fetchPlan {
	if len(haves) == 0 && len(shallowIn) == 0 && (depth == nil || *depth == 1) {
		sorted := append([]string(nil), wants...)
		sort.Strings(sorted)
		depthStr := ""
		if depth != nil {
			depthStr = strconv.Itoa(*depth)
		}
		key := strings.Join(sorted, ",") + "|" + depthStr
		r.cacheMu.RLock()
		hit, ok := r.packCache[key]
		r.cacheMu.RUnlock()
		if ok {
			var wantedRefs []struct{ oid, ref string }
			refNames := make([]string, 0, len(r.refs))
			for name := range r.refs {
				refNames = append(refNames, name)
			}
			sort.Strings(refNames)
			for _, name := range refNames {
				oid := r.refs[name]
				for _, w := range wants {
					if oid == w {
						wantedRefs = append(wantedRefs, struct{ oid, ref string }{w, name})
					}
				}
			}
			var shallow []string
			if depth != nil && *depth == 1 {
				for _, w := range wants {
					c := peelToCommit(r, w)
					if c == "" {
						continue
					}
					o, ok := r.objects[c]
					if ok && o.typ == objCommit && len(parseCommit(o.data).parents) > 0 {
						shallow = append(shallow, w)
					}
				}
			}
			return fetchPlan{pack: hit, shallow: shallow, wantedRefs: wantedRefs}
		}
		plan := computeFetch(r, wants, haves, shallowIn, depth)
		r.cacheMu.Lock()
		r.packCache[key] = plan.pack
		r.cacheMu.Unlock()
		return plan
	}
	return computeFetch(r, wants, haves, shallowIn, depth)
}

// fetchCacheKey is the cache key for a whole v2 fetch response, or "" when
// the shape is not cacheable (haves or shallow lines present, depth > 1).
func fetchCacheKey(wants []string, depth *int) string {
	if depth != nil && *depth != 1 {
		return ""
	}
	sorted := append([]string(nil), wants...)
	sort.Strings(sorted)
	depthStr := ""
	if depth != nil {
		depthStr = strconv.Itoa(*depth)
	}
	return strings.Join(sorted, ",") + "|" + depthStr
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

func refLinesV0(r *repo, service string) []byte {
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=" + agentName
	} else {
		caps = "symref=HEAD:refs/heads/" + r.defaultBranch + " side-band-64k multi_ack thin-pack ofs-delta agent=" + agentName
	}
	var out []byte
	headOID := r.tip(r.defaultBranch)
	if headOID == "" {
		headOID = zeroOID
	}
	out = append(out, pktLineStr("# service="+service+"\n")...)
	out = append(out, pktFlush...)
	out = append(out, pktLineStr(headOID+" HEAD\x00"+caps+"\n")...)
	refNames := make([]string, 0, len(r.refs))
	for name := range r.refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)
	for _, name := range refNames {
		oid := r.refs[name]
		out = append(out, pktLineStr(oid+" "+name+"\n")...)
		if obj, ok := r.objects[oid]; ok && obj.typ == objTag {
			target, _ := parseTag(obj.data)
			if target != "" {
				out = append(out, pktLineStr(target+" "+name+"^{}\n")...)
			}
		}
	}
	out = append(out, pktFlush...)
	return out
}

func advertisementV2(r *repo) []byte {
	var out []byte
	out = append(out, pktLineStr("# service=git-upload-pack\n")...)
	out = append(out, pktFlush...)
	out = append(out, pktLineStr("version 2\n")...)
	out = append(out, pktLineStr("agent="+agentName+"\n")...)
	out = append(out, pktLineStr("ls-refs=unborn peel symref\n")...)
	out = append(out, pktLineStr("fetch=shallow filter\n")...)
	out = append(out, pktLineStr("server-option\n")...)
	out = append(out, pktLineStr("object-format=sha1\n")...)
	out = append(out, pktFlush...)
	return out
}

func handleLsRefsV2(r *repo, args []string) []byte {
	peel, symrefs := false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out []byte
	headOID := r.tip(r.defaultBranch)
	if headOID == "" {
		if keep("HEAD") {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + r.defaultBranch
			}
			out = append(out, pktLineStr(line+"\n")...)
		}
	} else if keep("HEAD") {
		line := headOID + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + r.defaultBranch
		}
		out = append(out, pktLineStr(line+"\n")...)
	}
	refNames := make([]string, 0, len(r.refs))
	for name := range r.refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)
	for _, name := range refNames {
		oid := r.refs[name]
		if !keep(name) {
			continue
		}
		line := oid + " " + name
		if peel {
			if obj, ok := r.objects[oid]; ok && obj.typ == objTag {
				target, _ := parseTag(obj.data)
				if target != "" {
					line += " peeled:" + target
				}
			}
		}
		out = append(out, pktLineStr(line+"\n")...)
	}
	out = append(out, pktFlush...)
	return out
}

func handleFetchV2(r *repo, args []string) []byte {
	var wants, haves, shallow []string
	var depth *int
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			shallow = append(shallow, strings.TrimSpace(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(a[7:])); err == nil {
				depth = &d
			}
		}
	}
	// Whole-response cache: the 50 concurrent shallow clones of the bench
	// all send identical fetch commands, so the exact response bytes can
	// be reused. Only cacheable when there are no haves/shallow lines.
	var cacheKey string
	if len(haves) == 0 && len(shallow) == 0 {
		cacheKey = fetchCacheKey(wants, depth)
	}
	if cacheKey != "" {
		r.cacheMu.RLock()
		hit, ok := r.fetchCache[cacheKey]
		r.cacheMu.RUnlock()
		if ok {
			return hit
		}
	}
	plan := cachedFetch(r, wants, haves, shallow, depth)
	var out []byte
	out = append(out, pktLineStr("acknowledgments\n")...)
	if len(haves) == 0 {
		out = append(out, pktLineStr("NAK\n")...)
	} else {
		common := ""
		for _, w := range wants {
			for _, h := range haves {
				if w == h {
					common = w
				}
			}
		}
		if common != "" {
			out = append(out, pktLineStr("ACK "+common+"\n")...)
			out = append(out, pktLineStr("ready\n")...)
		} else {
			out = append(out, pktLineStr("NAK\n")...)
		}
	}
	out = append(out, pktFlush...)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		out = append(out, pktLineStr("shallow-info\n")...)
		for _, s := range plan.shallow {
			out = append(out, pktLineStr("shallow "+s+"\n")...)
		}
		for _, s := range plan.unshallow {
			out = append(out, pktLineStr("unshallow "+s+"\n")...)
		}
		out = append(out, pktFlush...)
	}
	if len(plan.wantedRefs) > 0 {
		out = append(out, pktLineStr("wanted-refs\n")...)
		for _, wr := range plan.wantedRefs {
			out = append(out, pktLineStr(wr.oid+" "+wr.ref+"\n")...)
		}
		out = append(out, pktFlush...)
	}
	out = append(out, pktLineStr("packfile\n")...)
	out = append(out, sidebandWrap(plan.pack)...)
	out = append(out, pktFlush...)
	if cacheKey != "" {
		r.cacheMu.Lock()
		r.fetchCache[cacheKey] = out
		r.cacheMu.Unlock()
	}
	return out
}

func handleFetchV0(r *repo, body []byte, pk []pkt) []byte {
	var wants, haves []string
	var depth *int
	for _, p := range pk {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "deepen "):
			if d, err := strconv.Atoi(strings.TrimSpace(line[7:])); err == nil {
				depth = &d
			}
		}
	}
	plan := computeFetch(r, wants, haves, nil, depth)
	out := pktLineStr("NAK\n")
	out = append(out, sidebandWrap(plan.pack)...)
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCmd struct{ oldOID, newOID, ref string }

func handleReceivePack(r *repo, body []byte) []byte {
	// Find the first flush packet: commands are before it, pack bytes after.
	packStart := -1
	pos := 0
	for pos+4 <= len(body) {
		n, err := strconv.ParseUint(string(body[pos:pos+4]), 16, 32)
		if err != nil {
			break
		}
		if n == 0 {
			packStart = pos + 4
			break
		}
		pos += int(n)
	}
	var packets []pkt
	if packStart >= 0 {
		packets = decodePktLines(body[:packStart])
	} else {
		packets = decodePktLines(body)
	}
	var commands []pushCmd
	for _, p := range packets {
		if p.kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if i := strings.IndexByte(line, 0); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 3 && len(fields[0]) == 40 && len(fields[1]) == 40 && strings.HasPrefix(fields[2], "refs/") {
			commands = append(commands, pushCmd{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
		}
	}
	type result struct {
		ref    string
		ok     bool
		reason string
	}
	var results []result
	unpackOK := true
	hasNew := false
	for _, c := range commands {
		if c.newOID != zeroOID {
			hasNew = true
		}
	}
	if hasNew {
		if packStart < 0 || packStart >= len(body) {
			unpackOK = false
			for _, c := range commands {
				results = append(results, result{c.ref, false, "unpacker error"})
			}
		} else {
			incoming, err := readPack(body[packStart:], r)
			if err != nil {
				unpackOK = false
				for _, c := range commands {
					results = append(results, result{c.ref, false, "unpacker error"})
				}
			} else {
				for oid, obj := range incoming {
					r.objects[oid] = obj
				}
				for _, c := range commands {
					_, curExists := r.refs[c.ref]
					if c.newOID == zeroOID {
						if !curExists {
							results = append(results, result{c.ref, false, "no such ref"})
							continue
						}
						if c.oldOID != zeroOID && c.oldOID != r.refs[c.ref] {
							results = append(results, result{c.ref, false, "old value does not match"})
							continue
						}
						delete(r.refs, c.ref)
						results = append(results, result{c.ref, true, ""})
						continue
					}
					if _, ok := r.objects[c.newOID]; !ok {
						results = append(results, result{c.ref, false, "object not found"})
						continue
					}
					if c.oldOID == zeroOID {
						if curExists {
							results = append(results, result{c.ref, false, "already exists"})
							continue
						}
					} else {
						if r.refs[c.ref] != c.oldOID {
							results = append(results, result{c.ref, false, "old value does not match"})
							continue
						}
						if !isAncestor(r, c.oldOID, c.newOID) {
							results = append(results, result{c.ref, false, "non-fast-forward"})
							continue
						}
					}
					r.refs[c.ref] = c.newOID
					results = append(results, result{c.ref, true, ""})
				}
				r.invalidate()
			}
		}
	} else {
		for _, c := range commands {
			_, curExists := r.refs[c.ref]
			if !curExists {
				results = append(results, result{c.ref, false, "no such ref"})
				continue
			}
			if c.oldOID != zeroOID && c.oldOID != r.refs[c.ref] {
				results = append(results, result{c.ref, false, "old value does not match"})
				continue
			}
			delete(r.refs, c.ref)
			results = append(results, result{c.ref, true, ""})
		}
		r.invalidate()
	}
	var inner []byte
	if unpackOK {
		inner = append(inner, pktLineStr("unpack ok\n")...)
	} else {
		inner = append(inner, pktLineStr("unpack error\n")...)
	}
	for _, res := range results {
		if res.ok {
			inner = append(inner, pktLineStr("ok "+res.ref+"\n")...)
		} else {
			inner = append(inner, pktLineStr("ng "+res.ref+" "+res.reason+"\n")...)
		}
	}
	out := sidebandWrap(inner)
	out = append(out, pktFlush...)
	return out
}

// ---------------------------------------------------------------------------
// Git router
// ---------------------------------------------------------------------------

// handleGit serves /:owner/:repo[.git]/info/refs|git-upload-pack|git-receive-pack.
// Returns (handled, response).
func handleGit(req *request, parts []string, query map[string]string) (bool, *response) {
	if len(parts) < 3 {
		return false, nil
	}
	owner := parts[0]
	name := parts[1]
	tail := strings.Join(parts[2:], "/")
	if tail != "info/refs" && tail != "git-upload-pack" && tail != "git-receive-pack" {
		return false, nil
	}
	if !authed(req) {
		return true, jsonErr(401, "Requires authentication")
	}
	db.mu.RLock()
	r := db.getRepo(owner, name)
	db.mu.RUnlock()
	if r == nil {
		return true, &response{status: 404, ctype: "text/plain", body: []byte("Not Found")}
	}
	if tail == "info/refs" {
		service := query["service"]
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return true, &response{status: 400, ctype: "text/plain", body: []byte("Invalid service")}
		}
		db.mu.RLock()
		var body []byte
		if service == "git-receive-pack" {
			body = refLinesV0(r, service)
		} else if req.gitProto == "version=2" {
			body = advertisementV2(r)
		} else {
			body = refLinesV0(r, service)
		}
		db.mu.RUnlock()
		ctype := "application/x-git-upload-pack-advertisement"
		if service == "git-receive-pack" {
			ctype = "application/x-git-receive-pack-advertisement"
		}
		return true, &response{status: 200, ctype: ctype, headers: [][2]string{{"Cache-Control", "no-cache"}}, body: body}
	}
	if tail == "git-receive-pack" {
		db.mu.Lock()
		out := handleReceivePack(r, req.body)
		db.mu.Unlock()
		return true, &response{status: 200, ctype: "application/x-git-receive-pack-result", headers: [][2]string{{"Cache-Control", "no-cache"}}, body: out}
	}
	// git-upload-pack
	pk := decodePktLines(req.body)
	isV2 := len(req.body) > 4
	if isV2 {
		isV2 = false
		for _, p := range pk {
			if p.kind == 'd' && strings.HasPrefix(string(p.payload), "command=") {
				isV2 = true
				break
			}
		}
	}
	if isV2 {
		var lines []string
		command := ""
		inArgs := false
		for _, p := range pk {
			if p.kind == 'l' {
				inArgs = true
				continue
			}
			if p.kind == 'f' {
				break
			}
			if p.kind != 'd' {
				continue
			}
			text := strings.TrimSuffix(string(p.payload), "\n")
			if !inArgs {
				if strings.HasPrefix(text, "command=") {
					command = text[8:]
				}
				continue
			}
			lines = append(lines, text)
		}
		var out []byte
		db.mu.RLock()
		switch command {
		case "ls-refs":
			out = handleLsRefsV2(r, lines)
		case "fetch":
			out = handleFetchV2(r, lines)
		default:
			db.mu.RUnlock()
			return true, &response{status: 400, ctype: "text/plain", body: []byte("unknown command")}
		}
		db.mu.RUnlock()
		return true, &response{status: 200, ctype: "application/x-git-upload-pack-result", headers: [][2]string{{"Cache-Control", "no-cache"}}, body: out}
	}
	db.mu.RLock()
	out := handleFetchV0(r, req.body, pk)
	db.mu.RUnlock()
	return true, &response{status: 200, ctype: "application/x-git-upload-pack-result", headers: [][2]string{{"Cache-Control", "no-cache"}}, body: out}
}
