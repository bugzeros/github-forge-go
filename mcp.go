// MCP: JSON-RPC 2.0 over Streamable HTTP. Speaks revision 2026-07-28
// (server/discover) and the legacy handshake revisions.
package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{Name: "create_repo", Description: "Create a new repository owned by the authenticated user.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "description": "Repository name"}, "description": map[string]any{"type": "string", "description": "Repository description"}, "private": map[string]any{"type": "boolean", "description": "Whether the repository is private"}}, "required": []string{"name"}}},
	{Name: "get_repo", Description: "Get a repository by owner and name.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}}, "required": []string{"owner", "repo"}}},
	{Name: "create_issue", Description: "Create an issue in a repository.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "title": map[string]any{"type": "string", "description": "Issue title"}, "body": map[string]any{"type": "string", "description": "Issue body"}, "labels": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Label names"}}, "required": []string{"owner", "repo", "title"}}},
	{Name: "list_issues", Description: "List issues in a repository.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "state": map[string]any{"type": "string", "description": "Issue state: open, closed, or all"}}, "required": []string{"owner", "repo"}}},
	{Name: "create_pull_request", Description: "Create a pull request in a repository.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "title": map[string]any{"type": "string", "description": "Pull request title"}, "body": map[string]any{"type": "string", "description": "Pull request body"}, "head": map[string]any{"type": "string", "description": "Head branch name"}, "base": map[string]any{"type": "string", "description": "Base branch name"}}, "required": []string{"owner", "repo", "title", "head", "base"}}},
	{Name: "merge_pull_request", Description: "Merge a pull request in a repository.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"owner": map[string]any{"type": "string", "description": "Repository owner login"}, "repo": map[string]any{"type": "string", "description": "Repository name"}, "pull_number": map[string]any{"type": "integer", "description": "Pull request number"}}, "required": []string{"owner", "repo", "pull_number"}}},
}

var mcpToolsListJSON []byte

func init() {
	b, _ := json.Marshal(map[string]any{"tools": mcpTools})
	mcpToolsListJSON = b
}

func mcpText(obj any) map[string]any {
	text := ""
	switch v := obj.(type) {
	case string:
		text = v
	default:
		b, _ := json.Marshal(obj)
		text = string(b)
	}
	out := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if m, ok := obj.(map[string]any); ok {
		out["structuredContent"] = m
	} else {
		out["structuredContent"] = map[string]any{"result": obj}
	}
	return out
}

func mcpErrObj(message string) map[string]any {
	b, _ := json.Marshal(map[string]any{"error": message})
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}, "isError": true}
}

func mcpRepoFromArgs(args map[string]any) *repo {
	owner := strVal(args["owner"])
	repoName := strVal(args["repo"])
	if repoName == "" {
		repoName = strVal(args["repository"])
	}
	if repoName == "" && strings.Contains(owner, "/") {
		parts := strings.SplitN(owner, "/", 2)
		owner, repoName = parts[0], parts[1]
	}
	if owner == "" {
		owner = viewerLogin
	}
	if repoName == "" {
		return nil
	}
	return db.getRepo(owner, repoName)
}

func mcpCall(name string, args map[string]any) map[string]any {
	switch name {
	case "create_repo":
		repoName := strVal(args["name"])
		if repoName == "" {
			return mcpErrObj("name is required")
		}
		if _, exists := db.repos[viewerLogin+"/"+repoName]; exists {
			return mcpErrObj("Repository already exists")
		}
		db.repoIDSeq++
		r := newRepo(db.repoIDSeq, repoName, strVal(args["description"]), args["private"] == true, db.nextSeq())
		db.repos[r.fullName()] = r
		return mcpText(repoJSON(r))
	case "get_repo":
		r := mcpRepoFromArgs(args)
		if r == nil {
			return mcpErrObj("Not Found")
		}
		return mcpText(repoJSON(r))
	case "create_issue":
		r := mcpRepoFromArgs(args)
		if r == nil {
			return mcpErrObj("Not Found")
		}
		title := strVal(args["title"])
		if title == "" {
			return mcpErrObj("title is required")
		}
		var labels []string
		if arr, ok := args["labels"].([]any); ok {
			for _, x := range arr {
				if s, ok := x.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		for _, l := range labels {
			if _, ok := r.labels[l]; !ok {
				return mcpErrObj("Label does not exist: " + l)
			}
		}
		n := db.nextSeq()
		db.issueIDSeq++
		iss := &issue{
			number: len(r.issues) + 1, id: db.issueIDSeq, title: title,
			body: strVal(args["body"]), state: "open", labels: labels,
			createdSeq: n, updatedSeq: n,
		}
		r.issues = append(r.issues, iss)
		r.issuesByNum[iss.number] = iss
		return mcpText(issueJSON(r, iss))
	case "list_issues":
		r := mcpRepoFromArgs(args)
		if r == nil {
			return mcpErrObj("Not Found")
		}
		state := strVal(args["state"])
		if state == "" {
			state = "open"
		}
		var list []map[string]any
		for _, i := range r.issues {
			if state == "all" || i.state == state {
				list = append(list, issueJSON(r, i))
			}
		}
		return mcpText(map[string]any{"issues": list})
	case "create_pull_request":
		r := mcpRepoFromArgs(args)
		if r == nil {
			return mcpErrObj("Not Found")
		}
		title := strVal(args["title"])
		head := strVal(args["head"])
		base := strVal(args["base"])
		if title == "" || head == "" || base == "" {
			return mcpErrObj("title, head and base are required")
		}
		headSha := r.tip(head)
		baseSha := r.tip(base)
		if headSha == "" || baseSha == "" {
			return mcpErrObj("head or base branch does not exist")
		}
		n := db.nextSeq()
		db.pullIDSeq++
		p := &pull{
			number: len(r.pulls) + 1, id: db.pullIDSeq, title: title,
			body: strVal(args["body"]), state: "open",
			headRef: head, headSha: headSha, baseRef: base, baseSha: baseSha,
			createdSeq: n, updatedSeq: n,
		}
		r.pulls = append(r.pulls, p)
		r.pullsByNum[p.number] = p
		return mcpText(pullJSON(r, p))
	case "merge_pull_request":
		r := mcpRepoFromArgs(args)
		if r == nil {
			return mcpErrObj("Not Found")
		}
		num := toInt(args["pull_number"])
		if num == 0 {
			num = toInt(args["number"])
		}
		p, ok := r.pullsByNum[num]
		if !ok {
			return mcpErrObj("Not Found")
		}
		sha, msg, status := mergePull(r, p)
		if status != 0 {
			return mcpErrObj(msg)
		}
		return mcpText(map[string]any{"sha": sha, "merged": true, "message": "Merge pull request #" + strconv.Itoa(p.number) + " from " + p.headRef})
	default:
		return mcpErrObj("unknown tool: " + name)
	}
}

// handleMcp serves POST /mcp.
func handleMcp(req *request) *response {
	rpc := func(id any, result any) *response {
		return jsonOK(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	rpcErr := func(id any, code int, message string, status int) *response {
		return jsonOK(status, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
	}
	var msg map[string]any
	if err := json.Unmarshal(req.body, &msg); err != nil || msg == nil {
		return rpcErr(nil, -32700, "Parse error", 400)
	}
	id := msg["id"]
	method, _ := msg["method"].(string)
	if method == "" || strings.HasPrefix(method, "notifications/") {
		return &response{status: 202, ctype: "application/json"}
	}
	if method == "initialize" {
		params, _ := msg["params"].(map[string]any)
		requested := strVal(params["protocolVersion"])
		supported := []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
		version := "2025-11-25"
		for _, v := range supported {
			if v == requested {
				version = v
			}
		}
		return rpc(id, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "github-forge", "version": "1.0.0"},
		})
	}
	if method == "server/discover" {
		return rpc(id, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			"serverInfo":        map[string]any{"name": "github-forge", "version": "1.0.0"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
		})
	}
	if !authed(req) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32001, "message": "Requires authentication"}})
		resp := jsonResp(401, b)
		return resp
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if method == "tools/list" {
		b := make([]byte, 0, len(mcpToolsListJSON)+64)
		b = append(b, []byte(`{"jsonrpc":"2.0","id":`)...)
		ib, _ := json.Marshal(id)
		b = append(b, ib...)
		b = append(b, []byte(`,"result":`)...)
		b = append(b, mcpToolsListJSON...)
		b = append(b, '}')
		return jsonResp(200, b)
	}
	if method == "tools/call" {
		params, _ := msg["params"].(map[string]any)
		toolName := strVal(params["name"])
		var tool *mcpTool
		for i := range mcpTools {
			if mcpTools[i].Name == toolName {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			return rpcErr(id, -32602, "unknown tool: "+toolName, 200)
		}
		args, ok := params["arguments"].(map[string]any)
		if !ok {
			return rpc(id, mcpErrObj("arguments are required"))
		}
		var required []string
		if req2, ok := tool.InputSchema["required"].([]string); ok {
			required = req2
		}
		var missing []string
		for _, k := range required {
			v := args[k]
			if v == nil || v == "" {
				missing = append(missing, k)
			}
		}
		if len(missing) > 0 {
			return rpc(id, mcpErrObj("missing required argument(s): "+strings.Join(missing, ", ")))
		}
		return rpc(id, mcpCall(toolName, args))
	}
	if method == "ping" {
		return rpc(id, map[string]any{})
	}
	return rpcErr(id, -32601, "Method not found: "+method, 200)
}
