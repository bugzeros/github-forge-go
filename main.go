// Entry point: one Go process that serves the forge and, when the make.fun
// suite runs beside it, hands the suite the port it picked and waits until
// the suite says it is done.
//
//   - In a trial pod, MAKEFUN_ENV_URL names the suite. The forge binds an
//     ephemeral port (port 0), tells the suite that port with a POST to
//     /register, then polls GET /done until the suite answers {"done": true}
//     and exits 0. It never binds 8080, where the suite listens.
//   - Anywhere else, it listens on a fixed port — a number on the command
//     line, the PORT variable, or 3000 — and serves until stopped.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// splitPath splits a decoded path into segments, like the Node parsePath.
func splitPath(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// matchIssueGet recognizes GET /api/v3/repos/{owner}/{repo}/issues/{number}
// directly on the path string, without allocating segment or query maps.
// This is the read workload's hot path (2,000 sequential GETs). It scans
// the four segments with indexByte instead of strings.Split, so the
// common case allocates nothing at all.
func matchIssueGet(path string) (owner, name, num string, ok bool) {
	const prefix = "/api/v3/repos/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", "", false
	}
	rest := path[len(prefix):]
	// owner/repo/issues/number — exactly 4 segments.
	s1 := indexByteStr(rest, '/')
	if s1 < 0 {
		return "", "", "", false
	}
	s2 := indexByteStr(rest[s1+1:], '/')
	if s2 < 0 {
		return "", "", "", false
	}
	s2 += s1 + 1
	s3 := indexByteStr(rest[s2+1:], '/')
	if s3 < 0 {
		return "", "", "", false
	}
	s3 += s2 + 1
	if indexByteStr(rest[s3+1:], '/') >= 0 {
		return "", "", "", false
	}
	if rest[s2+1:s3] != "issues" {
		return "", "", "", false
	}
	owner, name, num = rest[:s1], rest[s1+1:s2], rest[s3+1:]
	if owner == "" || name == "" || num == "" {
		return "", "", "", false
	}
	for i := 0; i < len(num); i++ {
		if num[i] < '0' || num[i] > '9' {
			return "", "", "", false
		}
	}
	return owner, name, num, true
}

// indexByteStr is indexByte over a string (no allocation).
func indexByteStr(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// fastIssueGet serves a cached issue GET under a read lock. It returns
// (nil, false) when the issue is not cached yet or does not exist, so the
// caller falls through to the normal path (which populates the cache under
// the write lock and answers 404s).
func fastIssueGet(owner, name, numStr string) ([]byte, bool) {
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return nil, false
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	r := db.getRepo(owner, name)
	if r == nil {
		return nil, false
	}
	iss := r.issuesByNum[n]
	if iss == nil {
		return nil, false
	}
	if iss.jsonCache != nil && iss.jsonCacheSeq == iss.updatedSeq && iss.jsonCacheNCom == len(iss.comments) {
		return iss.jsonCache, true
	}
	return nil, false
}

// dispatch routes one request to the right handler.
func dispatch(req *request) (resp *response) {
	defer func() {
		if rec := recover(); rec != nil {
			resp = jsonErr(500, "Internal error")
		}
	}()
	if req.path == "/healthz" {
		return jsonOK(200, map[string]any{"ok": true})
	}
	// Hot path: the read workload's 2,000 sequential issue GETs.
	if req.method == "GET" && authed(req) {
		if owner, name, num, ok := matchIssueGet(req.path); ok {
			if body, hit := fastIssueGet(owner, name, num); hit {
				return jsonBufResp(200, body)
			}
		}
	}
	parts := splitPath(req.path)
	if len(parts) >= 2 && parts[0] == "api" && parts[1] == "v3" {
		return handleRest(req, parts[2:], parseQuery(req.query))
	}
	if len(parts) == 2 && parts[0] == "api" && parts[1] == "graphql" {
		if req.method != "POST" {
			return jsonErr(404, "Not Found")
		}
		return handleGraphQL(req)
	}
	if len(parts) == 1 && parts[0] == "mcp" {
		if req.method != "POST" {
			return jsonErr(404, "Not Found")
		}
		return handleMcp(req)
	}
	// Git: /:owner/:repo[.git]/info/refs | git-upload-pack | git-receive-pack
	gitParts := append([]string(nil), parts...)
	if len(gitParts) >= 2 && strings.HasSuffix(gitParts[1], ".git") {
		gitParts[1] = gitParts[1][:len(gitParts[1])-4]
	}
	if handled, resp := handleGit(req, gitParts, parseQuery(req.query)); handled {
		return resp
	}
	return jsonErr(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func parsePort(text string) (int, bool) {
	if text == "" {
		return 0, false
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || n > 65535 {
		return 0, false
	}
	return n, true
}

func standalonePort() int {
	if len(os.Args) > 1 {
		if p, ok := parsePort(os.Args[1]); ok {
			return p
		}
	}
	if p, ok := parsePort(os.Getenv("PORT")); ok {
		return p
	}
	return 3000
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func registerWithSuite(envURL string, port int) error {
	target := envURL + "/register"
	body, _ := json.Marshal(map[string]any{"port": port})
	for attempt := 1; attempt <= 60; attempt++ {
		resp, err := httpClient.Post(target, "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			fmt.Fprintf(os.Stderr, "register attempt %d: the suite answered HTTP %d\n", attempt, resp.StatusCode)
		} else {
			fmt.Fprintf(os.Stderr, "register attempt %d: %v\n", attempt, err)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("could not register port %d with the suite at %s", port, envURL)
}

func waitForSuite(envURL string) {
	target := envURL + "/done"
	for {
		resp, err := httpClient.Get(target)
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if body.Done {
				return
			}
		}
		time.Sleep(time.Second)
	}
}

func main() {
	// The store is small (tens of MB) and the pod allows 1024 MB, so let
	// the heap grow further between collections: fewer GC cycles on the
	// timed workloads, at no risk to the memory limit.
	debug.SetGCPercent(400)
	suiteURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if suiteURL == "" {
		port := standalonePort()
		ln, err := net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
		if err != nil {
			fmt.Fprintf(os.Stderr, "github-forge failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("github-forge listening on http://127.0.0.1:%d\n", port)
		// Serve forever until stopped from outside.
		if err := serveListener(ln); err != nil {
			fmt.Fprintf(os.Stderr, "github-forge failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "github-forge failed: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("github-forge listening on port %d; registering with the suite\n", port)
	go func() {
		if err := serveListener(ln); err != nil {
			fmt.Fprintf(os.Stderr, "github-forge server error: %v\n", err)
			os.Exit(1)
		}
	}()
	if err := registerWithSuite(suiteURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "github-forge failed: %v\n", err)
		os.Exit(1)
	}
	waitForSuite(suiteURL)
	fmt.Printf("suite finished; shutting down\n")
	ln.Close()
}
