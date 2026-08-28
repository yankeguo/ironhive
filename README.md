# ironhive

Warm pools of sandbox containers on Kubernetes. `ironhive-controller` keeps a configurable number of standby pods per pool, hands them out over HTTP, and reverse-proxies each pod's in-container API; `ironhive-agent` runs as PID 1 inside every sandbox pod and exposes file / tar / dir / shell endpoints.

The controller's web UI is retro on the server, modern in the build:

- **std `net/http` only** — Go 1.22+ pattern routing (`GET /{$}`, `GET /static/`, `{id}` wildcards), security headers, graceful shutdown with no deadline. No web framework, no router dependency.
- **Bun multi-entry build** — every `.ts` / `.css` file in `web/src/entries/` is bundled by `web/build.ts` (`Bun.build`, IIFE, minified) into `web/dist/<name>-<hash>.<ext>`. `main.css` is a full Tailwind v4 build (`bun-plugin-tailwind`) with build-time lucide icons via `@iconify/tailwind4`.
- **`html/template` views** — embedded with `//go:embed`, referencing bundles only by entry name: `{{cssAsset "main"}}`, `{{jsAsset "home"}}`. Hash resolution happens in `web_static.go`.
- **Immutable static serving** — `web/dist` is embedded (`//go:embed all:web/dist`) and served at `GET /static/` with `Cache-Control: public, max-age=31536000, immutable`, so hashed assets are cached forever and new builds get new URLs.

## Layout

| Path | Role |
|---|---|
| `cmd/ironhive-controller/` | Controller binary: flag `-config` / `IHC_CONFIG` (default `config.yml`) — every other setting lives in the config file; graceful shutdown |
| `cmd/ironhive-agent/` | Agent binary: agent running inside managed containers |
| `controller/` | Controller package: HTTP server, pod manager, views, static assets |
| `controller/server.go` | `http.ServeMux` with method+path patterns, security headers, page handlers, allocate/release endpoints, `/agent/` reverse proxy |
| `controller/pods.go` | Pod manager: standby reconcile, list+watch in-memory state, allocate/release with cross-replica claims |
| `controller/kubernetes.go` | Kubernetes clientset: explicit kubeconfig → default loading rules → in-cluster fallback; namespace resolution |
| `controller/config.go` | `config.yml` loading: sections `http` (`listen`, default `:8080`), `kubernetes` (`kubeconfig`, `namespace`), and `pools.<name>` with `standby.static.count` (default 10) and `podTemplate` (`corev1.PodTemplateSpec`; the agent port is derived from its container ports) |
| `config.yml` | Example controller configuration with one `default` pool |
| `deploy/rbac.yaml` | Example RBAC: ServiceAccount + namespaced Role for pods, leader-election leases and events |
| `controller/web_tmpl.go` | `//go:embed web/view/*.html`, template funcs `jsAsset` / `cssAsset` |
| `controller/web_static.go` | `//go:embed all:web/dist`, `<entry>-<hash>.<ext>` matching, `/static/` handler |
| `controller/web/build.ts` | Bun build: bundles every entry in `src/entries/` into hashed IIFEs in `dist/` |
| `controller/web/src/entries/` | One file per bundle: page TS entries plus `main.css` (Tailwind v4) |
| `controller/web/view/` | Go templates; `base.html` defines shared `head` / `nav` blocks |
| `client.go`, `sandbox.go` | Root Go client package `ironhive`: controller endpoints (allocate/renew/release/pools) plus agent pass-through (file/tar/dir/shell) via the `Sandbox` handle |
| `agent/` | Agent package: agent logic for managed containers, PID 1 zombie reaping |

## ironhive-controller

`ironhive-controller` serves the web UI and drives the managed containers through the Kubernetes API. The only command-line flag is `-config` / `IHC_CONFIG` (default `config.yml`) — every other setting lives in the config file.

The config file is organized in sections: `http.listen` (HTTP listen address, default `:8080`), `kubernetes.kubeconfig` (explicit kubeconfig path; unset resolves the standard loading rules with the in-cluster config as fallback) and `kubernetes.namespace` (where managed pods live; default is the in-cluster service-account namespace, else `default`), plus the container pools: `pools.<name>.standby.static.count` (warm pods kept ready, default 10) and `pools.<name>.podTemplate` (a full Kubernetes pod template, parsed as `corev1.PodTemplateSpec`). The agent's listen port inside the pod is derived from the template's container ports: the one named `http-ironhive` wins, else the first declared port, else the default 19173. See the annotated `config.yml` in the repo root. An absent config file is tolerated (defaults, no pools); a present-but-invalid one fails startup.

Kubernetes credentials resolve in order: an explicit kubeconfig path, the default loading rules (`$KUBECONFIG`, then `~/.kube/config`), and the **in-cluster** service-account config as the fallback — inside a pod no configuration is needed at all. A malformed explicit kubeconfig fails hard rather than silently falling back. If no credentials resolve at startup the UI still serves and the failure is logged.

For in-cluster operation, `deploy/rbac.yaml` is a ready-to-apply example scoped to the `ironhive` namespace: a `ServiceAccount`, a `Role` granting pod get/list/watch/create/update/patch/delete plus coordination.k8s.io leases and events (leader election), and the `RoleBinding` between them. Set `serviceAccountName: ironhive-controller` on the controller's Deployment to pick it up.

### Pod manager

The pod manager (`controller/pods.go`) keeps each pool's standby pods warm and tracks every managed pod in memory:

- Pods are named `sandbox-<lowercase ULID>` and carry enforced labels — `app.kubernetes.io/managed-by=ironhive-controller` (also the list/watch selector), `ironhive.dev/pool=<pool name>`, and `ironhive.dev/template-hash` (a deterministic hash of the pool's `podTemplate` at creation time) — merged over any template labels. The controller-owned allocation annotations are stripped from templates when a standby pod is created.
- A list+watch loop maintains an in-memory map of pod states (phase, Ready condition, deletion, IP, allocation, lease deadline, template hash). It runs on **every** replica — it feeds the allocate fast path. A replica waits for its first successful list before joining leader election, and a broken/error watch is re-established from a fresh list with backoff.
- **Reconcile is single-writer**: replicas elect a leader through a `coordination.k8s.io` Lease named `ironhive-controller` in the managed namespace, and only the leader runs the reconcile loop — exact sizing, sweeps, lease reaping, template recycling. Every leader pass starts from its own authoritative List rather than a potentially lagging watch cache, so failover cannot duplicate a pool. The allocate / renew / release paths stay multi-replica: claims are serialized by the API server via the resourceVersion precondition, so they need no leader.
- Reconcile makes each pool converge to exactly `standby.static.count` (allocated and terminating pods don't count), preferentially removes surplus pods that are not Ready, sweeps `Succeeded` / `Failed` pods, destroys pods whose lease has expired, and recycles standby pods whose template hash no longer matches the config. Every sweep carries the observed `resourceVersion`, so a concurrent allocation or renewal wins instead of having its pod deleted. Allocated pods are never recycled early; their lease expiry reclaims them in time.
- **Allocation state lives on the pod object** as the `ironhive.dev/allocated` and `ironhive.dev/lease-expires` annotations. Claiming is a merge patch carrying the pod's `resourceVersion` as an optimistic-concurrency precondition, so racing controller replicas cannot claim the same pod — the API server accepts exactly one. No leader election; state survives controller restarts and is shared by all replicas through the watch.
- Every allocation carries a **lease**: the caller declares a duration at allocate time and extends it with `renew`; the next periodic reconcile after the deadline (normally within 30 seconds) destroys the pod, so a crashed caller can never leak a sandbox forever.
- Sandboxes are **single-use**: releasing (or lease expiry) destroys the pod and reconcile tops the pool up with a fresh one.
- Pod readiness is whatever the template declares: the example `config.yml` gates Ready on the agent's `/healthz` via a `readinessProbe`. Allocation additionally rejects terminating pods and pods left from an older template.

### API

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness probe, returns `{"message":"OK"}` |
| `POST /controller/v1/allocate?pool=&lease=` | Claim one Ready standby pod of the pool; blocks up to 30 s waiting for one to become available. `lease` is a mandatory Go duration string (`30s`, `5m`, `1h`) — the pod is destroyed when it expires unless renewed. Returns `{"sandbox":"<pod name>","leaseExpires":"<RFC3339>"}`; `400` for a missing/unknown pool or a missing/invalid lease, `503` when none became available in time |
| `POST /controller/v1/renew?sandbox=&lease=` | Extend an allocated pod's lease to `lease` from now. Returns `{"sandbox":"<pod name>","leaseExpires":"<RFC3339>"}`; `400` for a missing/invalid lease, `404` for an unknown or unallocated sandbox |
| `POST /controller/v1/release?sandbox=` | Destroy an allocated pod; the pool is topped up with a fresh standby pod asynchronously. Returns `{"released":"<pod name>"}`; `404` for an unknown or unallocated sandbox |
| `GET /controller/v1/pools` | Read-only cluster overview for the dashboard: per-pool `standby` / `pending` / `allocated` counts plus every managed pod with phase, Ready, deleting state, IP, allocation and lease deadline. Terminating pods remain visible but are excluded from capacity counts. Unauthenticated and CORS-open (`Access-Control-Allow-Origin: *`) by design |
| `ANY /agent/**` | Reverse-proxy to the agent inside the pod named by the `X-Sandbox-ID` request header (`http://<podIP>:<agentPort>`); the path is preserved — the agent serves its own API under `/agent/v1/...`. `404` when the sandbox is unknown, unallocated, terminating, or has no IP yet; `502` when the agent cannot be reached |

Parameter passing and response conventions follow the agent's: **POST** endpoints accept parameters in the query string, the urlencoded form body, or both (body entries win on conflicts); non-data responses (successes and errors alike) use the JSON envelope `{"message": ...}`.

### Dashboard

The home page is a read-only overview of the cluster: per-pool standby / pending / allocated counts and a live pod table (phase, readiness, IP, status, remaining lease, age), polling `GET /controller/v1/pools` every 3 s. It is unauthenticated and deliberately frameable — `X-Frame-Options` and CSP `frame-ancestors` are omitted so the page can be embedded into third-party systems via iframe. Deployment-level protection is an operator concern, layered in front of the controller.

## Go client

The root package (`import "github.com/yankeguo/ironhive"`) wraps the controller API and, through its reverse proxy, the agent API. `Allocate` returns a `Sandbox` handle carrying the pod name; renew, release and every agent call hang off it:

```go
c := ironhive.NewClient("http://ironhive-controller:8080")
sb, err := c.Allocate(ctx, "default", 5*time.Minute)
if err != nil { /* handle */ }
defer sb.Release(context.Background()) // use a cleanup context, not an expired work context

_ = sb.PutFile(ctx, "/tmp/input.txt", strings.NewReader("data"), nil)
err = sb.Shell(ctx, "wc -l /tmp/input.txt", nil, func(ev ironhive.ShellEvent) error {
	// ev.Type: stdout / stderr / error / exit / cwd / env
	return nil
})
```

Controller-level calls (`Renew`, `Release`, `Pools`) exist on `Client` too; `Sandbox.AgentDo` is the low-level escape hatch for anything the convenience methods (file / tar / dir / shell) don't cover. Built-in POST calls use urlencoded form bodies, while `AgentDo` preserves exactly the query and body supplied by its caller. Requests carry no client-side timeout — deadlines belong to the caller's context, matching the controller/agent philosophy. Non-2xx responses decode into `*ironhive.Error` from the `{"message": ...}` envelope. `Shell` reports command failures through `error` / non-zero `exit` events rather than its return error; a nil callback discards events while still waiting for completion.

## ironhive-agent

`ironhive-agent` is the agent running as the main process inside managed containers. Flags: `-listen` (default `:19173`) — command line only, no environment variables.

As **PID 1** it reaps orphaned zombies itself, so the image needs no tini — and `Dockerfile.agent` (a minimal busybox image) ships no `ENTRYPOINT` at all: it is only used as an initContainer that copies the binary into a shared `emptyDir`, from which the sandbox's main container launches it (see `config.yml`). Shell PIDs are registered as owned by `exec.Cmd`; the SIGCHLD reaper validates that procfs belongs to its PID namespace, enumerates direct children, and calls targeted `wait4(pid)` only for adopted orphans, so it cannot steal a shell's exit status. If procfs is unavailable, the conservative fallback waits for arbitrary children only while no managed shell is active.

### API

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness probe, returns `{"message":"OK"}` |
| `GET /agent/v1/file?path=` | Download a file as an attachment (`Range` supported). `path` may be absolute, or relative to the process working directory |
| `PUT /agent/v1/file?path=` | Upload a file **atomically**: the body lands in a temp file in the target directory, then is renamed over the target; missing parent directories are created automatically. With `url=` (http/https) the body is expected to be empty and the content is downloaded from that URL instead, with the same atomic write. Optional `chmod` (zero-prefixed octal, e.g. `0644`) and `chown` (`user:group`; names or numeric ids, either side omittable, e.g. `user`, `:group`, `1000:1000`) |
| `POST /agent/v1/file/upload?path=&url=` | Stream the local file at `path` as the request body to `url` (http/https). `method=` selects the outgoing method — only `PUT` / `POST` / `PATCH` (default `POST`); repeatable `headers=` attaches extra headers, each as `key=value`. A non-2xx upstream response is reported as `502` with a body snippet |
| `GET /agent/v1/tar?path=` | Stream a directory as an uncompressed tar attachment (`<dirname>.tar`); entry names are relative to the directory, so the archive round-trips through `PUT /agent/v1/tar`. Modes and mtimes preserved; symlinks and other special files are skipped. Optional repeatable `include=` / `exclude=` limit archived entries — patterns use `path.Match` syntax extended with `**` (crossing directories), matched against archive-relative names; excludes win, and excluding a directory drops its whole subtree |
| `PUT /agent/v1/tar?path=` | Extract an uncompressed tar stream into `path` (target directory and its parents are created if missing). With `url=` (http/https) the body is expected to be empty and the tar stream is downloaded from that URL instead. Optional repeatable `include=` / `exclude=` limit extracted entries, same pattern syntax as `GET /agent/v1/tar`; non-matching entries are skipped. Regular files and directories with preserved modes and mtimes; absolute entry names, `..` traversal and other entry types are rejected |
| `POST /agent/v1/tar/upload?path=&url=` | Pack the directory at `path` as an uncompressed tar stream (same archive as `GET /agent/v1/tar`, honoring the same repeatable `include=` / `exclude=` filters) and stream it as the request body to `url` with `Content-Type: application/x-tar`. `method=` / `headers=` behave as in `POST /agent/v1/file/upload`; the upload uses chunked encoding since the stream length is unknown upfront. A non-2xx upstream response is reported as `502` |
| `GET /agent/v1/dir?path=` | List a directory as a JSON array of `{name, dir, size, mode, mtime}`, sorted by name (`mode` is zero-prefixed octal, `mtime` RFC3339) |
| `PUT /agent/v1/dir?path=` | Create a directory like `mkdir -p`. Optional `chmod` / `chown`, same syntax as `PUT /agent/v1/file` (default mode `0755`) |
| `POST /agent/v1/shell` | Run the form field `command` via bash and stream output as server-sent events (see below). Optional form fields: `cwd` (working directory; absolute or relative to the process working directory), repeatable `env` (`KEY=VALUE` entries), and `strict_env` (boolean; see below). Calls are stateless and run concurrently |

Parameter passing convention: **PUT** endpoints take parameters only in the query string — the body is the data stream, or empty; **POST** endpoints accept parameters in the query string, the urlencoded form body, or both (body entries win on conflicts).

File operations on the same absolute path are serialized with a per-path mutex.

Endpoints that do not return data answer with a JSON envelope `{"message": ...}` (successes and errors alike, `Content-Type: application/json`), so fields can be added later without breaking clients.

### Shell sessions

`POST /agent/v1/shell` runs each command in a fresh bash — calls share nothing and run concurrently. The optional `cwd` field sets the working directory for that call only. The command's environment is assembled from two inputs:

- the repeatable `env` field (`KEY=VALUE` entries), and
- unless `strict_env=true`, a curated subset of the process environment — generic vars like `PATH` / `HOME` / `USER` / `LANG` / `LC_*` / `TERM` / `TZ` / `TMPDIR`, with platform-injected vars (Kubernetes `*_SERVICE_HOST` / `*_SERVICE_PORT`, credentials, pod metadata) deliberately dropped so sandboxed commands cannot observe their environment. With `strict_env=true` the command environment is exactly the `env` entries.

After the command exits, an EXIT trap snapshots its final pwd and environment, reported back as `cwd` / `env` events. The intended loop for the upstream harness: make the first call without `strict_env` and let the agent assemble a sane environment; read the reported `cwd` / `env`; then feed them back with `strict_env=true` on subsequent calls — from then on the session state is fully owned by the harness, with no dependence on the process environment.

The response is `text/event-stream`; `data` is JSON-encoded:

```
event: stdout
data: "hello"

event: stderr
data: "something failed"

event: exit
data: "0"

event: cwd
data: "/app"

event: env
data: {"HOME":"/root","PATH":"/usr/bin", ...}
```

One `stdout`/`stderr` event per output line; stream or spawn failures also produce an `error` event. A final `exit` event carries the exit code (128+signal when the command died to a signal, e.g. `143` for SIGTERM), followed by the `cwd` and `env` snapshots. The snapshots are absent when the command was `SIGKILL`ed before its EXIT trap ran.

If the client disconnects, the command's whole **process group** (bash plus any pipeline or subshell children) receives `SIGTERM` — the wrapper's `EXIT` trap still saves the state snapshot — and the process is `SIGKILL`ed after a 5-second grace period. Disconnecting is therefore a reliable cancel; a command whose descendants ignore `SIGTERM` may leave orphans behind, which are reparented to PID 1 and reaped when they eventually die.

### Upstream contract

The agent is deliberately low-level; the harness (timeouts, budget enforcement, session policy) lives upstream in the controller / LLM agent. The levers available to the upstream:

- **Cancel / timeout** — close the connection. There is no server-side timeout by design; the upstream enforces its own deadline and disconnects, which triggers the teardown described above.
- **Output capping** — read until you have enough, then disconnect. The agent streams unbounded output line by line and never truncates; the upstream decides when a command has said enough.
- **Background processes** — `nohup cmd &` (or plain `cmd &`) works: the handler does not hang on backgrounded grandchildren holding the output pipes open, and re-parented orphans are reaped by PID 1. Job tracking, if wanted, is upstream bookkeeping.
- **Session composition** — shell calls are stateless and parallel; the `cwd`/`env` events at the end of each stream report the command's final state, and the `cwd`/`env` form fields (plus `strict_env=true`) seed the next call. The harness decides whether calls share a session, fork one, or stay independent — LLMs tend to assume state persists between calls, and this is the mechanism to honor that assumption where wanted.
- **No size or time limits anywhere** — uploads, downloads and tar streams are unbounded. Quotas and limits are upstream or deployment concerns.
- **Security model** — these endpoints are unauthenticated remote code execution by design. The container network must isolate the agent so that only the controller can reach it; never publish the port.

## Develop

```bash
# terminal 1: rebuild bundles on change (unminified, inline sourcemaps)
(cd controller/web && bun install && bun run dev)

# terminal 2: run the controller
go run ./cmd/ironhive-controller
```

## Build

```bash
(cd controller/web && bun install --frozen-lockfile && bun run typecheck && bun run build)
gofmt -l . # must print nothing
go vet ./...
go test ./...
go test -race ./...
go build ./...
```

`controller/web/dist` is git-ignored (only `.gitkeep` is committed), so always run the frontend build before `go build` — in Docker, do it in an `oven/bun` stage.

## Release

`.github/workflows/release.yml` runs the full frontend and Go quality suite for pull requests and pushes. On `main` / tag pushes only, a successful quality job builds and pushes `ghcr.io/<owner>/<repo>` via the multi-stage `Dockerfile.controller` and `Dockerfile.agent`, with tags prefixed by component:

- push `main` → `controller-latest` / `agent-latest` and `controller-latest-<short_sha>` / `agent-latest-<short_sha>`
- push a git tag → `controller-<tag>` / `agent-<tag>`

Note: the bun stage mirrors the repo layout (`WORKDIR /repo/controller/web`, `COPY controller/*.go /repo/controller/`) because `main.css`'s Tailwind `@source "../../../*.go"` resolves relative to the CSS file — without the Go files next to `web/`, the glob lands on the container root and the build hangs scanning the whole filesystem.

## Adding a page

1. Add a route in `controller/server.go`, e.g. `mux.HandleFunc("GET /about", s.handleAbout)`.
2. Add a view `controller/web/view/about.html` with `{{template "head" .}}` and `<script src="{{jsAsset "about"}}" defer></script>`.
3. Add an entry `controller/web/src/entries/about.ts`.
4. `bun run build` — the new `about-<hash>.js` is picked up automatically.
