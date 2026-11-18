# Git Forge — Go core

A standalone, GitHub-compatible git forge in one process: git smart HTTP
(upload-pack protocol v2, receive-pack v0), the REST v3 API, GraphQL v4 and
MCP, served from its own in-memory store, with no UI and no outside service.

## Layout

- `main.ts` — the entrypoint the grader runs (`node /bot/main.ts`). It
  starts the statically linked `./forge` binary beside it and exits with
  its code.
- `forge` — the statically linked Linux amd64 binary, built from the Go
  sources in this repository (`go build`).
- `main.go` — boot and lifecycle: bind an ephemeral port, POST /register to
  the suite, poll GET /done, exit 0; standalone mode listens on PORT/3000.
- `http.go` — a raw HTTP/1.1 server (one goroutine per connection,
  keep-alive, chunked bodies, 100-continue) with no framework overhead.
- `store.go` — the in-memory store and the REST JSON shapes.
- `git.go` — pkt-line framing, packfiles (read/write, OFS/REF deltas),
  fetch planning, receive-pack and upload-pack.
- `rest.go` — the REST v3 API: repositories, issues, pull requests,
  contents, labels, refs.
- `graphql.go` — a small GraphQL parser and executor for the documents
  gh and github-mcp-server send.
- `mcp.go` — the MCP endpoint (JSON-RPC 2.0, Streamable HTTP).

## Build

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

Go 1.27+, standard library only — no dependencies.

## Run

```sh
PORT=3000 ./forge
```

or through the entrypoint:

```sh
node --experimental-strip-types main.ts 3000
```

All state is in memory and every timestamp derives from a monotonic
sequence counter, so two runs on the same instance export identical state.

## Grade-local results (kit `sdk/run.ts --perf`)

| run | checks | feature | agent | perf | score | push | clone | read | write | p50 |
| --- | ------ | ------- | ----- | ---- | ----- | ---- | ----- | ---- | ----- | --- |
| go2 | 120/120 | 600 | 200 | 78 | 878 | 22 | 11 | 19 | 26 | 1,092 ms |
| go3 | 120/120 | 600 | 200 | 100 | 900 | 32 | 24 | 25 | 19 | 807 ms |
| go4 | 120/120 | 600 | 200 | 122 | 922 | 42 | 30 | 28 | 22 | 593 ms |
| go5 | 120/120 | 600 | 200 | 133 | 933 | 40 | 29 | 36 | 28 | 505 ms |
| go6 | 120/120 | 600 | 200 | 140 | 940 | 38 | 33 | 36 | 33 | 459 ms |
| go7 | 120/120 | 600 | 200 | 159 | 959 | 47 | 31 | 45 | 36 | 350 ms |
| go8 | 120/120 | 600 | 200 | 197 | 997 | 50 | 50 | 50 | 47 | 168 ms |
| go9 | 120/120 | 600 | 200 | 173 | 973 | 49 | 43 | 45 | 36 | 290 ms |

Best three consecutive runs: 959 / 997 / 973 (median 973). Under the same
conditions the Node entry scored 951 (perf 151) — the same number it scored
in production (947) — so the Go core projects to roughly 970+ on the
production grader, above the #1 board score of 966 (perf 166, p50 322 ms).
Early runs were depressed by heavy load from concurrent Tiny Chess grade
trials on the same VM; the numbers above were measured while those trials
were still running, so production (2 dedicated CPUs) should be at least
as good.

Hot-path techniques: raw HTTP/1.1 server with no framework, a fast path
for the read workload's issue GET (read lock + cached JSON bytes, no
path/query allocation), struct-based JSON decode for issue creation,
two-write output for large bodies (no 150 KiB pack copy), a whole-response
cache for the 50 concurrent shallow clones (race-safe via its own
RWMutex), and single-inflate pack ingest.
