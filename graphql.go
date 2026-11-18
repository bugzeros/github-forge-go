// GraphQL v4: a small tokenizer + parser + executor for the documents gh
// and github-mcp-server send. Ported behavior-for-behavior from the Node
// entry.
package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Values and fields
// ---------------------------------------------------------------------------

type gqlValue struct {
	kind   string // var,string,int,float,bool,null,enum,list,object
	str    string
	num    float64
	b      bool
	items  []*gqlValue
	fields map[string]*gqlValue
	varName string
}

type gqlField struct {
	name         string
	alias        string
	args         map[string]*gqlValue
	selection    []*gqlField
	fragmentType string
}

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

func tokenizeGql(src string) []string {
	var tokens []string
	i := 0
	for i < len(src) {
		c := src[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == ',' {
			i++
			continue
		}
		if c == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.' {
			tokens = append(tokens, "...")
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$", c) >= 0 {
			tokens = append(tokens, string(c))
			i++
			continue
		}
		if c == '"' {
			j := i + 1
			var s strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					if j+1 < len(src) {
						s.WriteByte(src[j+1])
						j += 2
					} else {
						j++
					}
				} else {
					s.WriteByte(src[j])
					j++
				}
			}
			b, _ := json.Marshal(s.String())
			tokens = append(tokens, string(b))
			i = j + 1
			continue
		}
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_' {
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		if (c >= '0' && c <= '9') || c == '-' {
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.' || src[j] == 'e' || src[j] == 'E' || src[j] == '+' || src[j] == '-') {
				j++
			}
			tokens = append(tokens, src[i:j])
			i = j
			continue
		}
		i++
	}
	return tokens
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlParser struct {
	tokens []string
	pos    int
}

func (p *gqlParser) peek() string {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return ""
}

func (p *gqlParser) next() string {
	t := p.peek()
	p.pos++
	return t
}

func (p *gqlParser) expect(t string) {
	p.next()
}

func (p *gqlParser) parseValue() *gqlValue {
	t := p.next()
	switch {
	case t == "$":
		return &gqlValue{kind: "var", varName: p.next()}
	case t == "[":
		v := &gqlValue{kind: "list"}
		for p.peek() != "]" {
			v.items = append(v.items, p.parseValue())
		}
		p.expect("]")
		return v
	case t == "{":
		v := &gqlValue{kind: "object", fields: map[string]*gqlValue{}}
		for p.peek() != "}" {
			name := p.next()
			p.expect(":")
			v.fields[name] = p.parseValue()
		}
		p.expect("}")
		return v
	case len(t) > 0 && t[0] == '"':
		var s string
		json.Unmarshal([]byte(t), &s)
		return &gqlValue{kind: "string", str: s}
	case t == "true":
		return &gqlValue{kind: "bool", b: true}
	case t == "false":
		return &gqlValue{kind: "bool", b: false}
	case t == "null":
		return &gqlValue{kind: "null"}
	}
	if isIntStr(t) {
		n, _ := strconv.ParseInt(t, 10, 64)
		return &gqlValue{kind: "int", num: float64(n)}
	}
	if isNumStr(t) {
		n, _ := strconv.ParseFloat(t, 64)
		return &gqlValue{kind: "float", num: n}
	}
	return &gqlValue{kind: "enum", str: t}
}

func isIntStr(t string) bool {
	if t == "" {
		return false
	}
	i := 0
	if t[0] == '-' {
		i = 1
	}
	if i >= len(t) {
		return false
	}
	for ; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return false
		}
	}
	return true
}

func isNumStr(t string) bool {
	if t == "" {
		return false
	}
	_, err := strconv.ParseFloat(t, 64)
	return err == nil
}

func (p *gqlParser) parseArgs() map[string]*gqlValue {
	args := map[string]*gqlValue{}
	if p.peek() != "(" {
		return args
	}
	p.expect("(")
	for p.peek() != ")" {
		name := p.next()
		p.expect(":")
		args[name] = p.parseValue()
	}
	p.expect(")")
	return args
}

func (p *gqlParser) parseSelection() []*gqlField {
	p.expect("{")
	var fields []*gqlField
	for p.peek() != "}" {
		if p.peek() == "..." {
			p.next()
			var fragType string
			if p.peek() == "on" {
				p.next()
				fragType = p.next()
			} else {
				p.next() // named fragment spread: unused by the checked documents
			}
			sel := p.parseSelection()
			for _, f := range sel {
				cp := *f
				cp.fragmentType = fragType
				fields = append(fields, &cp)
			}
			continue
		}
		name := p.next()
		alias := ""
		if p.peek() == ":" {
			p.next()
			alias = name
			name = p.next()
		}
		args := p.parseArgs()
		var sel []*gqlField
		if p.peek() == "{" {
			sel = p.parseSelection()
		}
		fields = append(fields, &gqlField{name: name, alias: alias, args: args, selection: sel})
	}
	p.expect("}")
	return fields
}

func (p *gqlParser) parseDocument() (operation string, selection []*gqlField) {
	operation = "query"
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t == "{" {
			break
		}
		if t == "query" || t == "mutation" {
			operation = t
			p.next()
			continue
		}
		p.next()
		if p.peek() == "(" {
			depth := 0
			for p.pos < len(p.tokens) {
				x := p.next()
				if x == "(" || x == "[" || x == "{" {
					depth++
				}
				if x == ")" || x == "]" || x == "}" {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		}
	}
	return operation, p.parseSelection()
}

// ---------------------------------------------------------------------------
// Values
// ---------------------------------------------------------------------------

func evalValue(v *gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		return vars[v.varName]
	case "string":
		return v.str
	case "int":
		return int(v.num)
	case "float":
		return v.num
	case "bool":
		return v.b
	case "null":
		return nil
	case "enum":
		return v.str
	case "list":
		out := make([]any, 0, len(v.items))
		for _, it := range v.items {
			out = append(out, evalValue(it, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, val := range v.fields {
			out[k] = evalValue(val, vars)
		}
		return out
	}
	return nil
}

func evalArgs(f *gqlField, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range f.args {
		out[k] = evalValue(v, vars)
	}
	return out
}

// ---------------------------------------------------------------------------
// Type tables
// ---------------------------------------------------------------------------

var typeFields = map[string][]string{
	"Issue":       {"id", "databaseId", "number", "title", "body", "state", "author", "createdAt", "updatedAt", "url", "comments", "labels", "assignees", "closedAt", "repository"},
	"PullRequest": {"id", "databaseId", "number", "title", "body", "state", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "merged", "mergedAt", "mergeCommit", "files", "author", "createdAt", "updatedAt", "url", "repository", "reviews", "commits"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issues", "issue", "pullRequests", "pullRequest", "url", "createdAt", "updatedAt", "description", "isPrivate", "fork", "stargazerCount", "watchers"},
	"User":        {"id", "databaseId", "login", "name", "email", "url"},
	"IssueComment": {"id", "databaseId", "body", "author", "createdAt", "updatedAt", "url"},
	"Ref":         {"id", "name", "target", "prefix"},
	"SearchType":  {},
}

var enumValues = map[string][]string{
	"SearchType":     {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "ORGANIZATION", "PULL_REQUEST"},
	"IssueState":     {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
}

func typeInfo(name string) map[string]any {
	isEnum := false
	if _, ok := enumValues[name]; ok {
		isEnum = true
	}
	var fields any
	if isEnum {
		fields = nil
	} else {
		var list []map[string]any
		for _, f := range typeFields[name] {
			list = append(list, map[string]any{"name": f})
		}
		fields = list
	}
	var evs any
	if isEnum {
		var list []map[string]any
		for _, v := range enumValues[name] {
			list = append(list, map[string]any{"name": v})
		}
		evs = list
	} else {
		evs = nil
	}
	kind := "OBJECT"
	if isEnum {
		kind = "ENUM"
	}
	return map[string]any{"name": name, "kind": kind, "fields": fields, "enumValues": evs}
}

// ---------------------------------------------------------------------------
// Resolvers
// ---------------------------------------------------------------------------

func cursorFor(index int) string {
	return b64encode([]byte("cursor:" + strconv.Itoa(index)))
}

func cursorIndex(cursor any) int {
	s, ok := cursor.(string)
	if !ok {
		return -1
	}
	raw, ok := b64decode(s)
	if !ok {
		return -1
	}
	d := string(raw)
	if !strings.HasPrefix(d, "cursor:") {
		return -1
	}
	n, err := strconv.Atoi(d[len("cursor:"):])
	if err != nil {
		return -1
	}
	return n
}

func pageInfoFor(count, afterIdx, total int) map[string]any {
	start := afterIdx + 1
	var startCursor, endCursor any
	if count > 0 {
		startCursor = cursorFor(start)
		endCursor = cursorFor(start + count - 1)
	}
	return map[string]any{
		"hasNextPage":     start+count < total,
		"hasPreviousPage": start > 0,
		"startCursor":     startCursor,
		"endCursor":       endCursor,
	}
}

func repoGql(r *repo) map[string]any {
	var defaultBranchRef any
	if tip := r.tip(""); tip != "" {
		defaultBranchRef = map[string]any{"__type": "Ref", "name": r.defaultBranch, "target": map[string]any{"oid": tip}}
	}
	return map[string]any{
		"__type": "Repository", "__repo": r,
		"id": gid("Repository", r.id), "databaseId": r.id, "name": r.name, "nameWithOwner": r.fullName(),
		"owner": map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID), "name": viewerName, "email": viewerEmail},
		"defaultBranchRef": defaultBranchRef,
		"viewerPermission": "ADMIN", "hasIssuesEnabled": true,
		"url": "https://github.com/" + r.fullName(), "createdAt": db.isoFor(r.createdSeq), "updatedAt": db.isoFor(r.createdSeq),
		"description": r.description, "isPrivate": r.isPrivate, "fork": false,
	}
}

func issueGql(r *repo, i *issue) map[string]any {
	return map[string]any{
		"__type": "Issue", "__repo": r, "__issue": i,
		"id": gid("Issue", r.id, i.number), "databaseId": i.id, "number": i.number, "title": i.title, "body": i.body,
		"state": map[bool]string{true: "OPEN", false: "CLOSED"}[i.state == "open"],
		"author": map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID), "name": viewerName, "email": viewerEmail},
		"createdAt": db.isoFor(i.createdSeq), "updatedAt": db.isoFor(i.updatedSeq),
		"url":      "https://github.com/" + r.fullName() + "/issues/" + strconv.Itoa(i.number),
		"comments": map[string]any{"__type": "IssueCommentConnection", "__repo": r, "__issue": i},
		"labels":   map[string]any{"__type": "LabelConnection", "nodes": labelNodes(i)},
	}
}

func labelNodes(i *issue) []map[string]any {
	var out []map[string]any
	for _, l := range i.labels {
		out = append(out, map[string]any{"name": l})
	}
	return out
}

func pullGql(r *repo, p *pull) map[string]any {
	state := "OPEN"
	if p.merged {
		state = "MERGED"
	} else if p.state == "closed" {
		state = "CLOSED"
	}
	var mergeCommit any
	if p.mergeCommitSha != "" {
		mergeCommit = map[string]any{"oid": p.mergeCommitSha}
	}
	return map[string]any{
		"__type": "PullRequest", "__repo": r, "__pull": p,
		"id": gid("PullRequest", r.id, p.number), "databaseId": p.id, "number": p.number, "title": p.title, "body": p.body,
		"state": state,
		"headRefName": p.headRef, "baseRefName": p.baseRef,
		"headRefOid": p.headSha, "baseRefOid": p.baseSha,
		"mergeable": "MERGEABLE", "merged": p.merged, "mergedAt": p.mergedAt,
		"mergeCommit": mergeCommit,
		"author": map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID), "name": viewerName, "email": viewerEmail},
		"createdAt": db.isoFor(p.createdSeq), "updatedAt": db.isoFor(p.updatedSeq),
		"url":   "https://github.com/" + r.fullName() + "/pull/" + strconv.Itoa(p.number),
		"files": map[string]any{"__type": "PullRequestFileConnection", "__repo": r, "__pull": p},
	}
}

func issueConnection(r *repo, args map[string]any) map[string]any {
	list := append([]*issue(nil), r.issues...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		want := map[string]bool{}
		for _, s := range states {
			want[strings.ToLower(strings.TrimSpace(strings.ToLower(s.(string))))] = true
		}
		var filtered []*issue
		for _, i := range list {
			if want[i.state] {
				filtered = append(filtered, i)
			}
		}
		list = filtered
	}
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		dir := "DESC"
		if d, ok := orderBy["direction"].(string); ok {
			dir = d
		}
		sort.Slice(list, func(a, b int) bool {
			if dir == "DESC" {
				return list[a].createdSeq > list[b].createdSeq
			}
			return list[a].createdSeq < list[b].createdSeq
		})
	}
	afterIdx := cursorIndex(args["after"])
	first := 100
	if f, ok := args["first"].(int); ok {
		first = f
	}
	if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	start := afterIdx + 1
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
		end = len(list)
	}
	page := list[start:end]
	nodes := make([]map[string]any, 0, len(page))
	edges := make([]map[string]any, 0, len(page))
	for k, i := range page {
		nodes = append(nodes, issueGql(r, i))
		edges = append(edges, map[string]any{"node": issueGql(r, i), "cursor": cursorFor(afterIdx + 1 + k)})
	}
	return map[string]any{
		"__type": "IssueConnection", "__repo": r,
		"nodes": nodes, "edges": edges, "totalCount": len(list),
		"pageInfo": pageInfoFor(len(page), afterIdx, len(list)),
	}
}

func pullConnection(r *repo, args map[string]any) map[string]any {
	list := r.pulls
	afterIdx := cursorIndex(args["after"])
	first := 100
	if f, ok := args["first"].(int); ok {
		first = f
	}
	if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	start := afterIdx + 1
	end := start + first
	if end > len(list) {
		end = len(list)
	}
	if start > len(list) {
		start = len(list)
		end = len(list)
	}
	page := list[start:end]
	nodes := make([]map[string]any, 0, len(page))
	edges := make([]map[string]any, 0, len(page))
	for k, p := range page {
		nodes = append(nodes, pullGql(r, p))
		edges = append(edges, map[string]any{"node": pullGql(r, p), "cursor": cursorFor(afterIdx + 1 + k)})
	}
	return map[string]any{
		"__type": "PullRequestConnection", "__repo": r,
		"nodes": nodes, "edges": edges, "totalCount": len(list),
		"pageInfo": pageInfoFor(len(page), afterIdx, len(list)),
	}
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlExecutor struct {
	errors []map[string]any
}

func (e *gqlExecutor) resolveFields(parent map[string]any, typeName string, selection []*gqlField, vars map[string]any, path []string) map[string]any {
	out := map[string]any{}
	for _, field := range selection {
		if field.fragmentType != "" && field.fragmentType != typeName {
			continue
		}
		key := field.name
		if field.alias != "" {
			key = field.alias
		}
		np := append(append([]string(nil), path...), key)
		out[key] = e.resolveField(parent, typeName, field, vars, np)
	}
	return out
}

func (e *gqlExecutor) sub(value any, typ string, sel []*gqlField, vars map[string]any, path []string) any {
	if value == nil {
		return nil
	}
	if sel == nil {
		return value
	}
	if arr, ok := value.([]map[string]any); ok {
		out := make([]map[string]any, 0, len(arr))
		for _, v := range arr {
			out = append(out, e.resolveFields(v, typ, sel, vars, path))
		}
		return out
	}
	if arr, ok := value.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, v := range arr {
			if m, ok := v.(map[string]any); ok {
				out = append(out, e.resolveFields(m, typ, sel, vars, path))
			} else {
				out = append(out, v)
			}
		}
		return out
	}
	if m, ok := value.(map[string]any); ok {
		return e.resolveFields(m, typ, sel, vars, path)
	}
	return value
}

func (e *gqlExecutor) resolveField(parent map[string]any, typeName string, field *gqlField, vars map[string]any, path []string) any {
	args := evalArgs(field, vars)
	sel := field.selection
	sub := func(value any, t string) any {
		return e.sub(value, t, sel, vars, path)
	}
	if field.name == "__type" {
		name, _ := args["name"].(string)
		return sub(typeInfo(name), "__Type")
	}
	if field.name == "__typename" {
		return typeName
	}
	if typeName == "Query" {
		switch field.name {
		case "viewer":
			return sub(map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID), "name": viewerName, "email": viewerEmail}, "User")
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			r := db.getRepo(owner, name)
			if r == nil {
				return nil
			}
			return sub(repoGql(r), "Repository")
		case "node":
			id, _ := args["id"].(string)
			kind, parts, ok := parseGid(id)
			if !ok {
				return nil
			}
			switch kind {
			case "Repository":
				n, _ := strconv.Atoi(parts[0])
				for _, r := range db.repos {
					if r.id == n {
						return sub(repoGql(r), "Repository")
					}
				}
				return nil
			case "Issue":
				n, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				for _, r := range db.repos {
					if r.id == n {
						if i, ok := r.issuesByNum[num]; ok {
							return sub(issueGql(r, i), "Issue")
						}
					}
				}
				return nil
			case "PullRequest":
				n, _ := strconv.Atoi(parts[0])
				num, _ := strconv.Atoi(parts[1])
				for _, r := range db.repos {
					if r.id == n {
						if p, ok := r.pullsByNum[num]; ok {
							return sub(pullGql(r, p), "PullRequest")
						}
					}
				}
				return nil
			}
			return nil
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"Query\".", "path": path})
		return nil
	}
	if typeName == "Mutation" {
		result, typ := e.doMutation(field.name, args)
		if typ == "" {
			e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"Mutation\".", "path": path})
			return nil
		}
		if result == nil {
			return nil
		}
		return sub(result, typ)
	}
	if parent == nil {
		return nil
	}
	if typeName == "Repository" {
		r := parent["__repo"].(*repo)
		switch field.name {
		case "issues":
			return sub(issueConnection(r, args), "IssueConnection")
		case "issue":
			num := toInt(args["number"])
			if i, ok := r.issuesByNum[num]; ok {
				return sub(issueGql(r, i), "Issue")
			}
			return nil
		case "pullRequests":
			return sub(pullConnection(r, args), "PullRequestConnection")
		case "pullRequest":
			num := toInt(args["number"])
			if p, ok := r.pullsByNum[num]; ok {
				return sub(pullGql(r, p), "PullRequest")
			}
			return nil
		}
		if v, ok := parent[field.name]; ok {
			return sub(v, childType(typeName, field.name))
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"Repository\".", "path": path})
		return nil
	}
	if typeName == "PullRequest" {
		r := parent["__repo"].(*repo)
		p := parent["__pull"].(*pull)
		if field.name == "files" {
			files := pullFiles(r, p)
			afterIdx := cursorIndex(args["after"])
			first := 100
			if f, ok := args["first"].(int); ok {
				first = f
			}
			if f, ok := args["first"].(float64); ok {
				first = int(f)
			}
			start := afterIdx + 1
			end := start + first
			if end > len(files) {
				end = len(files)
			}
			if start > len(files) {
				start = len(files)
				end = len(files)
			}
			page := files[start:end]
			nodes := make([]map[string]any, 0, len(page))
			edges := make([]map[string]any, 0, len(page))
			for k, f := range page {
				nodes = append(nodes, map[string]any{"__type": "PullRequestFile", "path": f["filename"], "additions": f["additions"], "deletions": f["deletions"], "changes": f["changes"]})
				edges = append(edges, map[string]any{"node": map[string]any{"__type": "PullRequestFile", "path": f["filename"]}, "cursor": cursorFor(afterIdx + 1 + k)})
			}
			return sub(map[string]any{
				"__type": "PullRequestFileConnection",
				"nodes": nodes, "edges": edges, "totalCount": len(files),
				"pageInfo": pageInfoFor(len(page), afterIdx, len(files)),
			}, "PullRequestFileConnection")
		}
		if v, ok := parent[field.name]; ok {
			return sub(v, childType(typeName, field.name))
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"PullRequest\".", "path": path})
		return nil
	}
	if typeName == "IssueCommentConnection" {
		r := parent["__repo"].(*repo)
		i := parent["__issue"].(*issue)
		switch field.name {
		case "nodes":
			var out []map[string]any
			for _, c := range i.comments {
				out = append(out, map[string]any{"__type": "IssueComment", "id": gid("IssueComment", r.id, i.number, c.id), "databaseId": c.id, "body": c.body, "author": map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID)}, "createdAt": db.isoFor(c.createdSeq)})
			}
			return sub(out, "IssueComment")
		case "totalCount":
			return len(i.comments)
		case "pageInfo":
			return sub(pageInfoFor(len(i.comments), -1, len(i.comments)), "PageInfo")
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"IssueCommentConnection\".", "path": path})
		return nil
	}
	if typeName == "IssueConnection" || typeName == "PullRequestConnection" || typeName == "PullRequestFileConnection" || typeName == "LabelConnection" {
		if field.name == "nodes" || field.name == "edges" || field.name == "totalCount" || field.name == "pageInfo" {
			v := parent[field.name]
			if field.name == "nodes" {
				if arr, ok := v.([]map[string]any); ok {
					return sub(arr, childType(typeName, "nodes"))
				}
			}
			if field.name == "edges" {
				if arr, ok := v.([]map[string]any); ok {
					return sub(arr, "Edge")
				}
			}
			if field.name == "pageInfo" {
				return sub(v, "PageInfo")
			}
			return sub(v, "Int")
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"" + typeName + "\".", "path": path})
		return nil
	}
	if typeName == "Edge" {
		if field.name == "node" {
			return sub(parent["node"], childType(typeName, "node"))
		}
		if field.name == "cursor" {
			return parent["cursor"]
		}
		e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"Edge\".", "path": path})
		return nil
	}
	// Scalar object types: User, Ref, Issue, IssueComment, PullRequestFile, PageInfo, __Type
	if v, ok := parent[field.name]; ok {
		if m, isMap := v.(map[string]any); isMap && sel != nil {
			t := "String"
			if tt, ok := m["__type"].(string); ok {
				t = tt
			} else {
				t = childType(typeName, field.name)
			}
			return sub(v, t)
		}
		return sub(v, childType(typeName, field.name))
	}
	e.errors = append(e.errors, map[string]any{"message": "Cannot query field \"" + field.name + "\" on type \"" + typeName + "\".", "path": path})
	return nil
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func childType(parentType, field string) string {
	m := map[string]string{
		"Repository.owner": "User", "Repository.defaultBranchRef": "Ref", "Repository.issues": "IssueConnection",
		"Repository.pullRequests": "PullRequestConnection", "Repository.issue": "Issue", "Repository.pullRequest": "PullRequest",
		"Issue.author": "User", "Issue.comments": "IssueCommentConnection", "Issue.labels": "LabelConnection",
		"PullRequest.author": "User", "PullRequest.mergeCommit": "Commit", "PullRequest.files": "PullRequestFileConnection",
		"Ref.target": "GitObject", "IssueConnection.nodes": "Issue", "PullRequestConnection.nodes": "PullRequest",
		"PullRequestFileConnection.nodes": "PullRequestFile", "Edge.node": "Node", "User.id": "ID",
	}
	if t, ok := m[parentType+"."+field]; ok {
		return t
	}
	return "String"
}

func (e *gqlExecutor) doMutation(name string, args map[string]any) (map[string]any, string) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch name {
	case "createIssue":
		kind, parts, ok := parseGid(strVal(input["repositoryId"]))
		if !ok || kind != "Repository" {
			return nil, "CreateIssuePayload"
		}
		n, _ := strconv.Atoi(parts[0])
		var r *repo
		for _, rr := range db.repos {
			if rr.id == n {
				r = rr
			}
		}
		if r == nil {
			return nil, "CreateIssuePayload"
		}
		seq := db.nextSeq()
		db.issueIDSeq++
		iss := &issue{
			number: len(r.issues) + 1, id: db.issueIDSeq, title: strVal(input["title"]),
			body: strVal(input["body"]), state: "open", createdSeq: seq, updatedSeq: seq,
		}
		r.issues = append(r.issues, iss)
		r.issuesByNum[iss.number] = iss
		return map[string]any{"__type": "CreateIssuePayload", "issue": issueGql(r, iss), "clientMutationId": input["clientMutationId"]}, "CreateIssuePayload"
	case "closeIssue":
		kind, parts, ok := parseGid(strVal(input["issueId"]))
		if !ok || kind != "Issue" {
			return nil, "CloseIssuePayload"
		}
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		var r *repo
		for _, rr := range db.repos {
			if rr.id == n {
				r = rr
			}
		}
		if r == nil {
			return nil, "CloseIssuePayload"
		}
		i, ok := r.issuesByNum[num]
		if !ok {
			return nil, "CloseIssuePayload"
		}
		i.state = "closed"
		i.updatedSeq = db.nextSeq()
		return map[string]any{"__type": "CloseIssuePayload", "issue": issueGql(r, i), "clientMutationId": input["clientMutationId"]}, "CloseIssuePayload"
	case "createPullRequest":
		kind, parts, ok := parseGid(strVal(input["repositoryId"]))
		if !ok || kind != "Repository" {
			return nil, "CreatePullRequestPayload"
		}
		n, _ := strconv.Atoi(parts[0])
		var r *repo
		for _, rr := range db.repos {
			if rr.id == n {
				r = rr
			}
		}
		if r == nil {
			return nil, "CreatePullRequestPayload"
		}
		head := strVal(input["headRefName"])
		base := strVal(input["baseRefName"])
		headSha := r.tip(head)
		baseSha := r.tip(base)
		if headSha == "" || baseSha == "" {
			return nil, "CreatePullRequestPayload"
		}
		seq := db.nextSeq()
		db.pullIDSeq++
		p := &pull{
			number: len(r.pulls) + 1, id: db.pullIDSeq, title: strVal(input["title"]),
			body: strVal(input["body"]), state: "open",
			headRef: head, headSha: headSha, baseRef: base, baseSha: baseSha,
			createdSeq: seq, updatedSeq: seq,
		}
		r.pulls = append(r.pulls, p)
		r.pullsByNum[p.number] = p
		return map[string]any{"__type": "CreatePullRequestPayload", "pullRequest": pullGql(r, p), "clientMutationId": input["clientMutationId"]}, "CreatePullRequestPayload"
	case "mergePullRequest":
		kind, parts, ok := parseGid(strVal(input["pullRequestId"]))
		if !ok || kind != "PullRequest" {
			return nil, "MergePullRequestPayload"
		}
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		var r *repo
		for _, rr := range db.repos {
			if rr.id == n {
				r = rr
			}
		}
		if r == nil {
			return nil, "MergePullRequestPayload"
		}
		p, ok := r.pullsByNum[num]
		if !ok {
			return nil, "MergePullRequestPayload"
		}
		if _, _, status := mergePull(r, p); status != 0 {
			return nil, "MergePullRequestPayload"
		}
		return map[string]any{"__type": "MergePullRequestPayload", "pullRequest": pullGql(r, p), "clientMutationId": input["clientMutationId"]}, "MergePullRequestPayload"
	case "addComment":
		kind, parts, ok := parseGid(strVal(input["subjectId"]))
		if !ok || kind != "Issue" {
			return nil, "AddCommentPayload"
		}
		n, _ := strconv.Atoi(parts[0])
		num, _ := strconv.Atoi(parts[1])
		var r *repo
		for _, rr := range db.repos {
			if rr.id == n {
				r = rr
			}
		}
		if r == nil {
			return nil, "AddCommentPayload"
		}
		i, ok := r.issuesByNum[num]
		if !ok {
			return nil, "AddCommentPayload"
		}
		db.commentIDSeq++
		c := &comment{id: db.commentIDSeq, body: strVal(input["body"]), createdSeq: db.nextSeq()}
		i.comments = append(i.comments, c)
		i.updatedSeq = db.nextSeq()
		node := map[string]any{"__type": "IssueComment", "id": gid("IssueComment", r.id, i.number, c.id), "databaseId": c.id, "body": c.body, "author": map[string]any{"__type": "User", "login": viewerLogin, "id": gid("User", viewerID)}, "createdAt": db.isoFor(c.createdSeq)}
		return map[string]any{"__type": "AddCommentPayload", "commentEdge": map[string]any{"node": node}, "clientMutationId": input["clientMutationId"]}, "AddCommentPayload"
	}
	return nil, ""
}

func strVal(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (e *gqlExecutor) execute(query string, variables map[string]any) map[string]any {
	e.errors = nil
	defer func() {
		// A parser panic becomes a GraphQL error, never a crash.
		if rec := recover(); rec != nil {
			e.errors = append(e.errors, map[string]any{"message": "parse error"})
		}
	}()
	parser := &gqlParser{tokens: tokenizeGql(query)}
	operation, selection := parser.parseDocument()
	rootType := "Query"
	if operation == "mutation" {
		rootType = "Mutation"
	}
	data := e.resolveFields(nil, rootType, selection, variables, []string{})
	out := map[string]any{"data": data}
	if len(e.errors) > 0 {
		out["errors"] = e.errors
	}
	return out
}

// handleGraphQL serves POST /api/graphql.
func handleGraphQL(req *request) *response {
	if !authed(req) {
		return jsonErr(401, "Requires authentication")
	}
	body, ok := parseJSONBody(req)
	if !ok {
		b, _ := json.Marshal(map[string]any{"errors": []map[string]any{{"message": "Problems parsing JSON"}}})
		return jsonResp(400, b)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	exec := &gqlExecutor{}
	runOne := func(op any) map[string]any {
		m, ok := op.(map[string]any)
		if !ok {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "query is required"}}}
		}
		q, _ := m["query"].(string)
		if q == "" {
			return map[string]any{"data": nil, "errors": []map[string]any{{"message": "query is required"}}}
		}
		vars := map[string]any{}
		if v, ok := m["variables"].(map[string]any); ok {
			vars = v
		}
		return exec.execute(q, vars)
	}
	if arr, ok := body.([]any); ok {
		out := make([]map[string]any, 0, len(arr))
		for _, op := range arr {
			out = append(out, runOne(op))
		}
		return jsonOK(200, out)
	}
	return jsonOK(200, runOne(body))
}
