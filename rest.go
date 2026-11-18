// REST v3: repositories, issues, pull requests, contents, labels, refs.
package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
)

var token = os.Getenv("MAKEFUN_SHOP_TOKEN")

func authed(req *request) bool {
	if token == "" {
		return true
	}
	return req.auth == "Bearer "+token || req.auth == "token "+token
}

// jsonResp builds an application/json response with a fresh request id.
func jsonResp(status int, body []byte) *response {
	return &response{
		status: status, ctype: "application/json",
		headers: [][2]string{{"X-GitHub-Request-Id", "req-" + strconv.FormatInt(db.nextReq(), 10)}},
		body:    body,
	}
}

func jsonErr(status int, msg string) *response {
	b, _ := json.Marshal(map[string]any{"message": msg})
	return jsonResp(status, b)
}

func jsonOK(status int, v any) *response {
	b, _ := json.Marshal(v)
	return jsonResp(status, b)
}

func jsonBufResp(status int, buf []byte) *response {
	return jsonResp(status, buf)
}

// parseJSONBody parses the request body as JSON. Empty body -> empty object.
// Returns ok=false on malformed JSON.
func parseJSONBody(req *request) (any, bool) {
	raw := strings.TrimSpace(string(req.body))
	if raw == "" {
		return map[string]any{}, true
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, false
	}
	return v, true
}

func isRec(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func strField(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

func parseQuery(q string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(q, "&") {
		if pair == "" {
			continue
		}
		if i := strings.IndexByte(pair, '='); i >= 0 {
			out[queryUnescape(pair[:i])] = queryUnescape(pair[i+1:])
		} else {
			out[queryUnescape(pair)] = ""
		}
	}
	return out
}

func queryUnescape(s string) string {
	if !strings.Contains(s, "%") && !strings.Contains(s, "+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '+':
			b.WriteByte(' ')
		case '%':
			if i+2 < len(s) {
				if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 2
					continue
				}
			}
			b.WriteByte('%')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func pageParams(q map[string]string) (page, perPage int) {
	page = 1
	if v, err := strconv.Atoi(q["page"]); err == nil && v >= 1 {
		page = v
	}
	perPage = 30
	if v, err := strconv.Atoi(q["per_page"]); err == nil && v >= 1 {
		perPage = v
	}
	if perPage > 100 {
		perPage = 100
	}
	return page, perPage
}

// paginate slices items and, when a next page exists, adds the Link header.
func paginate(resp *response, total int, req *request, q map[string]string, path string) (start, end int) {
	page, perPage := pageParams(q)
	start = (page - 1) * perPage
	end = start + perPage
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	if start+perPage < total {
		host := req.host
		if host == "" {
			host = "127.0.0.1"
		}
		last := (total + perPage - 1) / perPage
		link := "<http://" + host + path + "?page=" + strconv.Itoa(page+1) + "&per_page=" + strconv.Itoa(perPage) + ">; rel=\"next\", " +
			"<http://" + host + path + "?page=" + strconv.Itoa(last) + "&per_page=" + strconv.Itoa(perPage) + ">; rel=\"last\""
		resp.header("Link", link)
	}
	return start, end
}

func validRepoName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// makeAPICommit builds a commit on the default branch from a flat file map.
func makeAPICommit(r *repo, message string, flat map[string]treeEntry, branch string) string {
	treeOID := buildTreeFromFlat(r, flat)
	parent := r.tip(branch)
	n := db.nextSeq()
	ts := baseEpoch + n
	ident := viewerName + " <" + viewerEmail + "> " + strconv.Itoa(ts) + " +0000"
	head := "tree " + treeOID + "\n"
	if parent != "" {
		head += "parent " + parent + "\n"
	}
	head += "author " + ident + "\ncommitter " + ident + "\n\n" + message + "\n"
	data := []byte(head)
	oid := objectID(objCommit, data)
	r.objects[oid] = &gitObj{typ: objCommit, data: data}
	r.refs["refs/heads/"+branch] = oid
	r.invalidate()
	return oid
}

func commitISO(r *repo, oid string) string {
	obj, ok := r.objects[oid]
	if !ok || obj.typ != objCommit {
		return db.isoFor(1)
	}
	cv := parseCommit(obj.data)
	// author line ends with " <ts> +0000"
	line := cv.author
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return db.isoFor(1)
	}
	tz := fields[len(fields)-1]
	tsStr := fields[len(fields)-2]
	if len(tz) != 5 || (tz[0] != '+' && tz[0] != '-') {
		return db.isoFor(1)
	}
	ts, err := strconv.Atoi(tsStr)
	if err != nil {
		return db.isoFor(1)
	}
	return db.isoUnix(ts)
}

func commitJSON(r *repo, oid string) map[string]any {
	obj, _ := r.objects[oid]
	var cv commitView
	if obj != nil {
		cv = parseCommit(obj.data)
	}
	// Strip the trailing " <ts> <tz>" from the author line, then "name <email>".
	author := cv.author
	fields := strings.Fields(author)
	if len(fields) >= 2 {
		author = strings.Join(fields[:len(fields)-2], " ")
	}
	name, email := viewerName, viewerEmail
	if lt := strings.Index(author, "<"); lt >= 0 {
		name = strings.TrimSpace(author[:lt])
		if gt := strings.Index(author, ">"); gt > lt {
			email = author[lt+1 : gt]
		}
	} else if author != "" {
		name = author
	}
	parents := make([]map[string]any, 0, len(cv.parents))
	for _, p := range cv.parents {
		parents = append(parents, map[string]any{"sha": p})
	}
	return map[string]any{
		"sha": oid,
		"commit": map[string]any{
			"message": strings.TrimSpace(cv.message),
			"author":  map[string]any{"name": name, "email": email, "date": commitISO(r, oid)},
			"committer": map[string]any{"name": name, "email": email, "date": commitISO(r, oid)},
		},
		"parents":   parents,
		"author":    map[string]any{"login": viewerLogin, "id": viewerID},
		"committer": map[string]any{"login": viewerLogin, "id": viewerID},
	}
}

func pullFiles(r *repo, p *pull) []map[string]any {
	baseFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	if obj, ok := r.objects[p.baseSha]; ok && obj.typ == objCommit {
		flattenTree(r, parseCommit(obj.data).tree, "", baseFlat)
	}
	if headTip := r.tip(p.headRef); headTip != "" {
		if obj, ok := r.objects[headTip]; ok && obj.typ == objCommit {
			flattenTree(r, parseCommit(obj.data).tree, "", headFlat)
		}
	}
	paths := map[string]bool{}
	for k := range baseFlat {
		paths[k] = true
	}
	for k := range headFlat {
		paths[k] = true
	}
	var sorted []string
	for k := range paths {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var out []map[string]any
	for _, path := range sorted {
		b, hasB := baseFlat[path]
		h, hasH := headFlat[path]
		if hasB && hasH && b.oid == h.oid {
			continue
		}
		status := "modified"
		additions, deletions := 0, 0
		if !hasB && hasH {
			status = "added"
			if blob, ok := r.objects[h.oid]; ok {
				additions = countLines(blob.data)
			}
		} else if hasB && !hasH {
			status = "removed"
			if blob, ok := r.objects[b.oid]; ok {
				deletions = countLines(blob.data)
			}
		} else if hasB && hasH {
			bb := ""
			hb := ""
			if blob, ok := r.objects[b.oid]; ok {
				bb = string(blob.data)
			}
			if blob, ok := r.objects[h.oid]; ok {
				hb = string(blob.data)
			}
			bl := strings.Split(bb, "\n")
			hl := strings.Split(hb, "\n")
			bSet := map[string]bool{}
			for _, l := range bl {
				bSet[l] = true
			}
			hSet := map[string]bool{}
			for _, l := range hl {
				hSet[l] = true
			}
			for _, l := range hl {
				if !bSet[l] {
					additions++
				}
			}
			for _, l := range bl {
				if !hSet[l] {
					deletions++
				}
			}
			if additions == 0 && deletions == 0 {
				additions = 1
			}
		}
		out = append(out, map[string]any{"filename": path, "status": status, "additions": additions, "deletions": deletions, "changes": additions + deletions})
	}
	return out
}

// countLines mirrors the Node counting: newlines + 1 when non-empty.
func countLines(data []byte) int {
	n := 0
	for _, c := range data {
		if c == '\n' {
			n++
		}
	}
	if len(data) > 0 {
		n++
	}
	return n
}

func mergePull(r *repo, p *pull) (string, string, int) {
	if p.merged {
		return "", "Pull request already merged", 405
	}
	baseTip := r.tip(p.baseRef)
	headTip := r.tip(p.headRef)
	if baseTip == "" || headTip == "" {
		return "", "Not Found", 404
	}
	mergeBase := findMergeBase(r, baseTip, headTip)
	if mergeBase == "" {
		mergeBase = p.baseSha
	}
	baseFlat := map[string]treeEntry{}
	headFlat := map[string]treeEntry{}
	curFlat := map[string]treeEntry{}
	if obj, ok := r.objects[mergeBase]; ok && obj.typ == objCommit {
		flattenTree(r, parseCommit(obj.data).tree, "", baseFlat)
	}
	if obj, ok := r.objects[headTip]; ok && obj.typ == objCommit {
		flattenTree(r, parseCommit(obj.data).tree, "", headFlat)
	}
	if obj, ok := r.objects[baseTip]; ok && obj.typ == objCommit {
		flattenTree(r, parseCommit(obj.data).tree, "", curFlat)
	}
	for path, hEnt := range headFlat {
		if bEnt, ok := baseFlat[path]; ok && bEnt.oid != hEnt.oid {
			if cEnt, ok := curFlat[path]; ok && cEnt.oid != bEnt.oid && cEnt.oid != hEnt.oid {
				return "", "Merge conflict", 405
			}
		}
	}
	for path, bEnt := range baseFlat {
		if _, ok := headFlat[path]; !ok {
			if cEnt, ok := curFlat[path]; ok && cEnt.oid != bEnt.oid {
				return "", "Merge conflict", 405
			}
		}
	}
	merged := map[string]treeEntry{}
	for k, v := range curFlat {
		merged[k] = v
	}
	for k, v := range headFlat {
		merged[k] = v
	}
	for k := range baseFlat {
		if _, ok := headFlat[k]; !ok {
			delete(merged, k)
		}
	}
	n := db.nextSeq()
	treeOID := buildTreeFromFlat(r, merged)
	ts := baseEpoch + n
	ident := viewerName + " <" + viewerEmail + "> " + strconv.Itoa(ts) + " +0000"
	head := "tree " + treeOID + "\nparent " + baseTip + "\nparent " + headTip + "\nauthor " + ident + "\ncommitter " + ident + "\n\n" + p.title + "\n"
	data := []byte(head)
	oid := objectID(objCommit, data)
	r.objects[oid] = &gitObj{typ: objCommit, data: data}
	r.refs["refs/heads/"+p.baseRef] = oid
	r.invalidate()
	p.state = "closed"
	p.merged = true
	p.mergedAt = db.isoFor(n)
	p.mergeCommitSha = oid
	p.updatedSeq = n
	return oid, "", 0
}

// ---------------------------------------------------------------------------
// REST router
// ---------------------------------------------------------------------------

// handleRest serves /api/v3/...; seg is the path segments after api/v3.
func handleRest(req *request, seg []string, q map[string]string) *response {
	if !authed(req) {
		return jsonErr(401, "Requires authentication")
	}
	method := req.method
	if len(seg) == 0 {
		if method != "GET" {
			return jsonErr(404, "Not Found")
		}
		resp := jsonOK(200, map[string]any{"current_user_url": "/api/v3/user", "repository_url": "/api/v3/repos/{owner}/{repo}"})
		resp.header("X-OAuth-Scopes", "repo, read:org")
		return resp
	}
	if seg[0] == "meta" && len(seg) == 1 {
		return jsonOK(200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": true, "github_services_sha": zeroOID})
	}
	if seg[0] == "user" && len(seg) == 1 {
		return jsonOK(200, map[string]any{"login": viewerLogin, "id": viewerID, "name": viewerName, "email": viewerEmail, "type": "User"})
	}
	if seg[0] == "user" && len(seg) > 1 && seg[1] == "repos" {
		if method == "POST" {
			body, ok := parseJSONBody(req)
			if !ok {
				return jsonErr(400, "Problems parsing JSON")
			}
			m, _ := isRec(body)
			name := strField(m, "name")
			if !validRepoName(name) {
				return jsonErr(422, "name is required and must be a valid repository name")
			}
			db.mu.Lock()
			defer db.mu.Unlock()
			if _, exists := db.repos[viewerLogin+"/"+name]; exists {
				return jsonErr(422, "Repository already exists")
			}
			db.repoIDSeq++
			r := newRepo(db.repoIDSeq, name, strField(m, "description"), m["private"] == true, db.nextSeq())
			db.repos[r.fullName()] = r
			return jsonOK(201, repoJSON(r))
		}
		if method == "GET" {
			db.mu.RLock()
			all := make([]*repo, 0, len(db.repos))
			for _, r := range db.repos {
				all = append(all, r)
			}
			sort.Slice(all, func(a, b int) bool { return all[a].createdSeq < all[b].createdSeq })
			resp := jsonResp(200, nil)
			start, end := paginate(resp, len(all), req, q, req.path)
			list := make([]map[string]any, 0, end-start)
			for _, r := range all[start:end] {
				list = append(list, repoJSON(r))
			}
			b, _ := json.Marshal(list)
			resp.body = b
			db.mu.RUnlock()
			return resp
		}
		return jsonErr(404, "Not Found")
	}
	if seg[0] == "repos" && len(seg) >= 3 {
		owner, name := seg[1], seg[2]
		rest := seg[3:]
		db.mu.Lock()
		defer db.mu.Unlock()
		r := db.getRepo(owner, name)
		if r == nil {
			return jsonErr(404, "Not Found")
		}
		if len(rest) == 0 {
			switch method {
			case "GET":
				return jsonOK(200, repoJSON(r))
			case "PATCH":
				body, ok := parseJSONBody(req)
				if !ok {
					return jsonErr(400, "Problems parsing JSON")
				}
				m, _ := isRec(body)
				if s := strField(m, "description"); m["description"] != nil {
					r.description = s
				}
				if v, ok := m["private"].(bool); ok {
					r.isPrivate = v
				}
				if s := strField(m, "name"); validRepoName(s) && s != r.name {
					delete(db.repos, r.fullName())
					r.name = s
					db.repos[r.fullName()] = r
				}
				return jsonOK(200, repoJSON(r))
			case "DELETE":
				delete(db.repos, r.fullName())
				return jsonResp(204, nil)
			}
			return jsonErr(404, "Not Found")
		}
		if rest[0] == "branches" && len(rest) == 1 && method == "GET" {
			names := make([]string, 0, len(r.refs))
			for k := range r.refs {
				if strings.HasPrefix(k, "refs/heads/") {
					names = append(names, k)
				}
			}
			sort.Strings(names)
			out := make([]map[string]any, 0, len(names))
			for _, k := range names {
				out = append(out, map[string]any{"name": k[len("refs/heads/"):], "commit": map[string]any{"sha": r.refs[k]}, "protected": false})
			}
			return jsonOK(200, out)
		}
		if rest[0] == "commits" && method == "GET" {
			if len(rest) == 2 {
				obj, ok := r.objects[rest[1]]
				if !ok || obj.typ != objCommit {
					return jsonErr(404, "Not Found")
				}
				return jsonOK(200, commitJSON(r, rest[1]))
			}
			ref := q["sha"]
			if ref == "" {
				ref = r.defaultBranch
			}
			tip := r.refs["refs/heads/"+ref]
			if tip == "" {
				if _, ok := r.objects[ref]; ok {
					tip = ref
				}
			}
			if tip == "" {
				return jsonOK(200, []any{})
			}
			list := commitWalk(r, tip, 100)
			resp := jsonResp(200, nil)
			start, end := paginate(resp, len(list), req, q, req.path)
			items := make([]map[string]any, 0, end-start)
			for _, oid := range list[start:end] {
				items = append(items, commitJSON(r, oid))
			}
			b, _ := json.Marshal(items)
			resp.body = b
			return resp
		}
		if rest[0] == "contents" {
			return handleContents(req, r, rest[1:], q)
		}
		if rest[0] == "git" && len(rest) >= 2 && rest[1] == "refs" {
			if method == "POST" && len(rest) == 2 {
				body, ok := parseJSONBody(req)
				if !ok {
					return jsonErr(400, "Problems parsing JSON")
				}
				m, _ := isRec(body)
				ref := strField(m, "ref")
				sha := strField(m, "sha")
				if !strings.HasPrefix(ref, "refs/") || !hasObject(r, sha) {
					return jsonErr(422, "ref and sha are required and sha must exist")
				}
				if _, exists := r.refs[ref]; exists {
					return jsonErr(422, "Reference already exists")
				}
				r.refs[ref] = sha
				r.invalidate()
				typ := "commit"
				if o, ok := r.objects[sha]; ok {
					typ = typeName(o.typ)
				}
				return jsonOK(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": typ}, "url": ""})
			}
			if method == "GET" && len(rest) >= 3 {
				ref := "refs/" + strings.Join(rest[2:], "/")
				oid, ok := r.refs[ref]
				if !ok {
					return jsonErr(404, "Not Found")
				}
				typ := "commit"
				if o, ok := r.objects[oid]; ok {
					typ = typeName(o.typ)
				}
				return jsonOK(200, map[string]any{"ref": ref, "object": map[string]any{"sha": oid, "type": typ}, "url": ""})
			}
			return jsonErr(404, "Not Found")
		}
		if rest[0] == "labels" {
			if method == "GET" && len(rest) == 1 {
				names := make([]string, 0, len(r.labels))
				for n := range r.labels {
					names = append(names, n)
				}
				sort.Strings(names)
				out := make([]map[string]any, 0, len(names))
				for _, n := range names {
					l := r.labels[n]
					out = append(out, map[string]any{"id": l.id, "name": l.name, "color": l.color, "description": ""})
				}
				return jsonOK(200, out)
			}
			if method == "POST" && len(rest) == 1 {
				body, ok := parseJSONBody(req)
				if !ok {
					return jsonErr(400, "Problems parsing JSON")
				}
				m, _ := isRec(body)
				name := strField(m, "name")
				if name == "" {
					return jsonErr(422, "name is required")
				}
				if _, exists := r.labels[name]; exists {
					return jsonErr(422, "Label already exists")
				}
				db.labelIDSeq++
				color := strField(m, "color")
				if color == "" {
					color = "ededed"
				}
				l := &label{name: name, color: color, id: db.labelIDSeq}
				r.labels[name] = l
				return jsonOK(201, map[string]any{"id": l.id, "name": name, "color": color, "description": ""})
			}
			return jsonErr(404, "Not Found")
		}
		if rest[0] == "issues" {
			return handleIssues(req, r, rest[1:], q)
		}
		if rest[0] == "pulls" {
			return handlePulls(req, r, rest[1:], q)
		}
	}
	return jsonErr(404, "Not Found")
}

func hasObject(r *repo, oid string) bool {
	_, ok := r.objects[oid]
	return ok
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

func handleContents(req *request, r *repo, rest []string, q map[string]string) *response {
	path := strings.Join(rest, "/")
	method := req.method
	ref := q["ref"]
	if ref == "" {
		ref = r.defaultBranch
	}
	tip := r.refs["refs/heads/"+ref]
	if tip == "" {
		if _, ok := r.objects[ref]; ok {
			tip = ref
		}
	}
	if method == "GET" {
		if tip == "" {
			return jsonErr(404, "Not Found")
		}
		tipObj, ok := r.objects[tip]
		if !ok || tipObj.typ != objCommit {
			return jsonErr(404, "Not Found")
		}
		flat := map[string]treeEntry{}
		flattenTree(r, parseCommit(tipObj.data).tree, "", flat)
		if path == "" {
			return jsonOK(200, rootListing(r, flat))
		}
		if ent, ok := flat[path]; ok {
			blob := r.objects[ent.oid]
			size := 0
			content := ""
			if blob != nil {
				size = len(blob.data)
				content = base64.StdEncoding.EncodeToString(blob.data)
			}
			base := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				base = path[i+1:]
			}
			return jsonOK(200, map[string]any{
				"type": "file", "encoding": "base64", "size": size, "name": base,
				"path": path, "sha": ent.oid, "content": content, "url": "", "download_url": nil,
			})
		}
		prefix := path + "/"
		var children []string
		for p := range flat {
			if strings.HasPrefix(p, prefix) {
				children = append(children, p)
			}
		}
		if len(children) > 0 {
			return jsonOK(200, dirListing(r, flat, path))
		}
		return jsonErr(404, "Not Found")
	}
	if method == "PUT" || method == "DELETE" {
		body, ok := parseJSONBody(req)
		if !ok {
			return jsonErr(400, "Problems parsing JSON")
		}
		m, _ := isRec(body)
		branch := strField(m, "branch")
		if branch == "" {
			branch = r.defaultBranch
		}
		curTip := r.tip(branch)
		curFlat := map[string]treeEntry{}
		if curTip != "" {
			if o, ok := r.objects[curTip]; ok && o.typ == objCommit {
				flattenTree(r, parseCommit(o.data).tree, "", curFlat)
			}
		}
		if method == "PUT" {
			contentStr, isStr := m["content"].(string)
			if !isStr {
				return jsonErr(422, "content is required")
			}
			content, err := base64.StdEncoding.DecodeString(contentStr)
			if err != nil {
				return jsonErr(422, "content is required")
			}
			blobOID := objectID(objBlob, content)
			r.objects[blobOID] = &gitObj{typ: objBlob, data: content}
			existing, hasExisting := curFlat[path]
			if sha, ok := m["sha"].(string); ok {
				if !hasExisting || existing.oid != sha {
					return jsonErr(409, "sha does not match the current file")
				}
			}
			curFlat[path] = treeEntry{mode: "100644", oid: blobOID}
			message := strField(m, "message")
			if message == "" {
				message = "Update " + path
			}
			commitSha := makeAPICommit(r, message, curFlat, branch)
			status := 201
			if hasExisting {
				status = 200
			}
			base := path
			if i := strings.LastIndex(path, "/"); i >= 0 {
				base = path[i+1:]
			}
			return jsonOK(status, map[string]any{
				"content": map[string]any{"type": "file", "encoding": "base64", "size": len(content), "name": base, "path": path, "sha": blobOID, "content": contentStr},
				"commit":  map[string]any{"sha": commitSha, "message": message},
			})
		}
		// DELETE
		if _, ok := curFlat[path]; !ok {
			return jsonErr(404, "Not Found")
		}
		if sha, ok := m["sha"].(string); ok && curFlat[path].oid != sha {
			return jsonErr(409, "sha does not match the current file")
		}
		delete(curFlat, path)
		message := strField(m, "message")
		if message == "" {
			message = "Delete " + path
		}
		commitSha := makeAPICommit(r, message, curFlat, branch)
		return jsonOK(200, map[string]any{"commit": map[string]any{"sha": commitSha, "message": message}})
	}
	return jsonErr(404, "Not Found")
}

func rootListing(r *repo, flat map[string]treeEntry) []map[string]any {
	seen := map[string]map[string]any{}
	var order []string
	for p, ent := range flat {
		first := p
		if i := strings.Index(p, "/"); i >= 0 {
			first = p[:i]
		}
		if _, ok := seen[first]; !ok {
			isDir := strings.Contains(p, "/")
			typ := "file"
			sha := ent.oid
			if isDir {
				typ = "dir"
				sha = ""
			}
			seen[first] = map[string]any{"type": typ, "name": first, "path": first, "sha": sha}
			order = append(order, first)
		}
	}
	sort.Strings(order)
	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		e := seen[name]
		if e["type"] == "dir" {
			dirFlat := map[string]treeEntry{}
			for p, ent := range flat {
				if strings.HasPrefix(p, name+"/") {
					dirFlat[p[len(name)+1:]] = ent
				}
			}
			sha := buildTreeFromFlat(r, dirFlat)
			out = append(out, map[string]any{"type": "dir", "name": name, "path": name, "sha": sha, "size": 0, "url": "", "download_url": nil})
		} else {
			blob := r.objects[e["sha"].(string)]
			size := 0
			if blob != nil {
				size = len(blob.data)
			}
			out = append(out, map[string]any{"type": "file", "name": name, "path": name, "sha": e["sha"], "size": size, "url": "", "download_url": nil})
		}
	}
	return out
}

func dirListing(r *repo, flat map[string]treeEntry, path string) []map[string]any {
	prefix := path + "/"
	seen := map[string]map[string]any{}
	var order []string
	for p, ent := range flat {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		restPath := p[len(prefix):]
		first := restPath
		if i := strings.Index(restPath, "/"); i >= 0 {
			first = restPath[:i]
		}
		if _, ok := seen[first]; !ok {
			isDir := strings.Contains(restPath, "/")
			typ := "file"
			sha := ent.oid
			if isDir {
				typ = "dir"
				sha = ""
			}
			seen[first] = map[string]any{"type": typ, "name": first, "path": path + "/" + first, "sha": sha}
			order = append(order, first)
		}
	}
	sort.Strings(order)
	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		e := seen[name]
		if e["type"] == "dir" {
			dirFlat := map[string]treeEntry{}
			for p, ent := range flat {
				if strings.HasPrefix(p, e["path"].(string)+"/") {
					dirFlat[p[len(e["path"].(string))+1:]] = ent
				}
			}
			sha := buildTreeFromFlat(r, dirFlat)
			out = append(out, map[string]any{"type": "dir", "name": name, "path": e["path"], "sha": sha, "size": 0, "url": "", "download_url": nil})
		} else {
			blob := r.objects[e["sha"].(string)]
			size := 0
			if blob != nil {
				size = len(blob.data)
			}
			out = append(out, map[string]any{"type": "file", "name": name, "path": e["path"], "sha": e["sha"], "size": size, "url": "", "download_url": nil})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Issues
// ---------------------------------------------------------------------------

func handleIssues(req *request, r *repo, rest []string, q map[string]string) *response {
	method := req.method
	if len(rest) == 0 {
		if method == "GET" {
			state := q["state"]
			if state == "" {
				state = "open"
			}
			list := r.issues
			if state != "all" {
				var filtered []*issue
				for _, i := range list {
					if i.state == state {
						filtered = append(filtered, i)
					}
				}
				list = filtered
			}
			sortField := q["sort"]
			if sortField == "" {
				sortField = "created"
			}
			dir := q["direction"]
			if dir == "" {
				dir = "desc"
			}
			if sortField == "created" {
				sorted := append([]*issue(nil), list...)
				sort.Slice(sorted, func(a, b int) bool {
					if dir == "asc" {
						return sorted[a].createdSeq < sorted[b].createdSeq
					}
					return sorted[a].createdSeq > sorted[b].createdSeq
				})
				list = sorted
			}
			resp := jsonResp(200, nil)
			start, end := paginate(resp, len(list), req, q, req.path)
			items := make([]map[string]any, 0, end-start)
			for _, i := range list[start:end] {
				items = append(items, issueJSON(r, i))
			}
			b, _ := json.Marshal(items)
			resp.body = b
			return resp
		}
		if method == "POST" {
			var in struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if len(req.body) > 0 {
				if err := json.Unmarshal(req.body, &in); err != nil {
					return jsonErr(400, "Problems parsing JSON")
				}
			}
			if in.Title == "" {
				return jsonErr(422, "title is required")
			}
			for _, l := range in.Labels {
				if _, ok := r.labels[l]; !ok {
					return jsonErr(422, "Label does not exist: "+l)
				}
			}
			n := db.nextSeq()
			db.issueIDSeq++
			iss := &issue{
				number: len(r.issues) + 1, id: db.issueIDSeq, title: in.Title,
				body: in.Body, state: "open", labels: in.Labels,
				createdSeq: n, updatedSeq: n,
			}
			r.issues = append(r.issues, iss)
			r.issuesByNum[iss.number] = iss
			return jsonOK(201, issueJSON(r, iss))
		}
		return jsonErr(404, "Not Found")
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	iss := r.issuesByNum[num]
	if len(rest) == 1 {
		if iss == nil {
			return jsonErr(404, "Not Found")
		}
		if method == "GET" {
			return jsonBufResp(200, issueJSONBytes(r, iss))
		}
		if method == "PATCH" {
			body, ok := parseJSONBody(req)
			if !ok {
				return jsonErr(400, "Problems parsing JSON")
			}
			m, _ := isRec(body)
			if st, ok := m["state"].(string); ok && (st == "open" || st == "closed") {
				iss.state = st
			}
			if s, ok := m["title"].(string); ok {
				iss.title = s
			}
			if s, ok := m["body"].(string); ok {
				iss.body = s
			}
			if arr, ok := m["labels"].([]any); ok {
				var labels []string
				for _, x := range arr {
					if s, ok := x.(string); ok {
						labels = append(labels, s)
					}
				}
				for _, l := range labels {
					if _, ok := r.labels[l]; !ok {
						return jsonErr(422, "Label does not exist: "+l)
					}
				}
				iss.labels = labels
			}
			iss.updatedSeq = db.nextSeq()
			return jsonBufResp(200, issueJSONBytes(r, iss))
		}
		return jsonErr(404, "Not Found")
	}
	if len(rest) == 2 && rest[1] == "comments" {
		if iss == nil {
			return jsonErr(404, "Not Found")
		}
		if method == "GET" {
			out := make([]map[string]any, 0, len(iss.comments))
			for _, c := range iss.comments {
				out = append(out, map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": viewerLogin, "id": viewerID}, "created_at": db.isoFor(c.createdSeq), "updated_at": db.isoFor(c.createdSeq)})
			}
			return jsonOK(200, out)
		}
		if method == "POST" {
			body, ok := parseJSONBody(req)
			if !ok {
				return jsonErr(400, "Problems parsing JSON")
			}
			m, _ := isRec(body)
			cbody, ok := m["body"].(string)
			if !ok || cbody == "" {
				return jsonErr(422, "body is required")
			}
			db.commentIDSeq++
			c := &comment{id: db.commentIDSeq, body: cbody, createdSeq: db.nextSeq()}
			iss.comments = append(iss.comments, c)
			iss.updatedSeq = db.nextSeq()
			return jsonOK(201, map[string]any{"id": c.id, "body": c.body, "user": map[string]any{"login": viewerLogin, "id": viewerID}, "created_at": db.isoFor(c.createdSeq), "updated_at": db.isoFor(c.createdSeq)})
		}
		return jsonErr(404, "Not Found")
	}
	return jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Pulls
// ---------------------------------------------------------------------------

func stripOwnerPrefix(s string) string {
	if i := strings.Index(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func handlePulls(req *request, r *repo, rest []string, q map[string]string) *response {
	method := req.method
	if len(rest) == 0 {
		if method == "GET" {
			state := q["state"]
			if state == "" {
				state = "open"
			}
			var list []*pull
			for _, p := range r.pulls {
				if state == "all" || p.state == state {
					list = append(list, p)
				}
			}
			resp := jsonResp(200, nil)
			start, end := paginate(resp, len(list), req, q, req.path)
			items := make([]map[string]any, 0, end-start)
			for _, p := range list[start:end] {
				items = append(items, pullJSON(r, p))
			}
			b, _ := json.Marshal(items)
			resp.body = b
			return resp
		}
		if method == "POST" {
			body, ok := parseJSONBody(req)
			if !ok {
				return jsonErr(400, "Problems parsing JSON")
			}
			m, _ := isRec(body)
			title := strField(m, "title")
			head := stripOwnerPrefix(strField(m, "head"))
			base := stripOwnerPrefix(strField(m, "base"))
			if title == "" || head == "" || base == "" {
				return jsonErr(422, "title, head and base are required")
			}
			headSha := r.tip(head)
			baseSha := r.tip(base)
			if headSha == "" || baseSha == "" {
				return jsonErr(422, "head or base branch does not exist")
			}
			n := db.nextSeq()
			db.pullIDSeq++
			p := &pull{
				number: len(r.pulls) + 1, id: db.pullIDSeq, title: title,
				body: strField(m, "body"), state: "open",
				headRef: head, headSha: headSha, baseRef: base, baseSha: baseSha,
				createdSeq: n, updatedSeq: n,
			}
			r.pulls = append(r.pulls, p)
			r.pullsByNum[p.number] = p
			return jsonOK(201, pullJSON(r, p))
		}
		return jsonErr(404, "Not Found")
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil {
		return jsonErr(404, "Not Found")
	}
	p := r.pullsByNum[num]
	if len(rest) == 1 {
		if p == nil {
			return jsonErr(404, "Not Found")
		}
		if method == "GET" {
			return jsonOK(200, pullJSON(r, p))
		}
		if method == "PATCH" {
			body, ok := parseJSONBody(req)
			if !ok {
				return jsonErr(400, "Problems parsing JSON")
			}
			m, _ := isRec(body)
			if st, ok := m["state"].(string); ok && (st == "open" || st == "closed") {
				p.state = st
			}
			if s, ok := m["title"].(string); ok {
				p.title = s
			}
			if s, ok := m["body"].(string); ok {
				p.body = s
			}
			p.updatedSeq = db.nextSeq()
			return jsonOK(200, pullJSON(r, p))
		}
		return jsonErr(404, "Not Found")
	}
	if len(rest) == 2 && rest[1] == "files" && method == "GET" {
		if p == nil {
			return jsonErr(404, "Not Found")
		}
		return jsonOK(200, pullFiles(r, p))
	}
	if len(rest) == 2 && rest[1] == "merge" && method == "PUT" {
		if p == nil {
			return jsonErr(404, "Not Found")
		}
		sha, msg, status := mergePull(r, p)
		if status != 0 {
			return jsonErr(status, msg)
		}
		return jsonOK(200, map[string]any{"sha": sha, "merged": true, "message": "Merge pull request #" + strconv.Itoa(p.number) + " from " + p.headRef})
	}
	return jsonErr(404, "Not Found")
}
