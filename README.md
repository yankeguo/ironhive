# ironhive

A GitHub template that bundles multiple TypeScript entrypoints and Tailwind CSS with Bun, then serves the hashed assets through classic Go `html/template` and `net/http`.

Retro on the server, modern in the build:

- **std `net/http` only** — Go 1.22+ pattern routing (`GET /{$}`, `GET /static/`, `{id}` wildcards), security headers, graceful shutdown with no deadline. No web framework, no router dependency.
- **Bun multi-entry build** — every `.ts` / `.css` file in `web/src/entries/` is bundled by `web/build.ts` (`Bun.build`, IIFE, minified) into `web/dist/<name>-<hash>.<ext>`. `main.css` is a full Tailwind v4 build (`bun-plugin-tailwind`) with build-time lucide icons via `@iconify/tailwind4`.
- **`html/template` views** — embedded with `//go:embed`, referencing bundles only by entry name: `{{cssAsset "main"}}`, `{{jsAsset "home"}}`. Hash resolution happens in `web_static.go`.
- **Immutable static serving** — `web/dist` is embedded (`//go:embed all:web/dist`) and served at `GET /static/` with `Cache-Control: public, max-age=31536000, immutable`, so hashed assets are cached forever and new builds get new URLs.

## Layout

| Path | Role |
|---|---|
| `cmd/ironhive-controller/` | Controller binary: flags (`-listen` / `IHC_LISTEN`, default `:8080`; `-kubeconfig` / `IHC_KUBECONFIG`), graceful shutdown |
| `cmd/ironhive-agent/` | Agent binary: agent running inside managed containers |
| `controller/` | Controller package: HTTP server, views, static assets |
| `controller/server.go` | `http.ServeMux` with method+path patterns, security headers, page handlers |
| `controller/kubernetes.go` | Kubernetes clientset: explicit kubeconfig → default loading rules → in-cluster fallback |
| `deploy/rbac.yaml` | Example RBAC: ServiceAccount + Role (pods get/list/watch/create/update/patch/delete) in the `ironhive` namespace |
| `controller/web_tmpl.go` | `//go:embed web/view/*.html`, template funcs `jsAsset` / `cssAsset` |
| `controller/web_static.go` | `//go:embed all:web/dist`, `<entry>-<hash>.<ext>` matching, `/static/` handler |
| `controller/web/build.ts` | Bun build: bundles every entry in `src/entries/` into hashed IIFEs in `dist/` |
| `controller/web/src/entries/` | One file per bundle: page TS entries plus `main.css` (Tailwind v4) |
| `controller/web/view/` | Go templates; `base.html` defines shared `head` / `nav` blocks |
| `agent/` | Agent package: agent logic for managed containers, PID 1 zombie reaping |

## ironhive-controller

`ironhive-controller` serves the web UI and drives the managed containers through the Kubernetes API. Flags: `-listen` / `IHC_LISTEN` (default `:8080`), `-kubeconfig` / `IHC_KUBECONFIG`.

Kubernetes credentials resolve in order: an explicit kubeconfig path, the default loading rules (`$KUBECONFIG`, then `~/.kube/config`), and the **in-cluster** service-account config as the fallback — inside a pod no configuration is needed at all. A malformed explicit kubeconfig fails hard rather than silently falling back. If no credentials resolve at startup the UI still serves and the failure is logged.

For in-cluster operation, `deploy/rbac.yaml` is a ready-to-apply example scoped to the `ironhive` namespace: a `ServiceAccount`, a `Role` granting pod get/list/watch/create/update/patch/delete, and the `RoleBinding` between them. Set `serviceAccountName: ironhive-controller` on the controller's Deployment to pick it up.

## ironhive-agent

`ironhive-agent` is the agent running as the main process inside managed containers. Flags: `-listen` / `IHA_LISTEN` (default `:19173`).

As **PID 1** it reaps orphaned zombies itself (SIGCHLD-driven `wait4(-1)`), so the image needs no tini — `Dockerfile.agent` uses the binary directly as `ENTRYPOINT`.

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

One `stdout`/`stderr` event per output line, then a final `exit` event with the exit code (128+signal when the command died to a signal, e.g. `143` for SIGTERM), then the `cwd` and `env` snapshots. The snapshots are absent when the command was `SIGKILL`ed before its EXIT trap ran.

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
(cd controller/web && bun run typecheck && bun run build)
go test ./...
go build ./cmd/ironhive-controller ./cmd/ironhive-agent
```

`controller/web/dist` is git-ignored (only `.gitkeep` is committed), so always run the frontend build before `go build` — in Docker, do it in an `oven/bun` stage.

## Release

`.github/workflows/release.yml` builds and pushes `ghcr.io/<owner>/<repo>` via the multi-stage `Dockerfile.controller` and `Dockerfile.agent`, with tags prefixed by component:

- push `main` → `controller-latest` / `agent-latest` and `controller-latest-<short_sha>` / `agent-latest-<short_sha>`
- push a git tag → `controller-<tag>` / `agent-<tag>`

Note: the bun stage mirrors the repo layout (`WORKDIR /repo/controller/web`, `COPY controller/*.go /repo/controller/`) because `main.css`'s Tailwind `@source "../../../*.go"` resolves relative to the CSS file — without the Go files next to `web/`, the glob lands on the container root and the build hangs scanning the whole filesystem.

## Adding a page

1. Add a route in `controller/server.go`, e.g. `mux.HandleFunc("GET /about", s.handleAbout)`.
2. Add a view `controller/web/view/about.html` with `{{template "head" .}}` and `<script src="{{jsAsset "about"}}" defer></script>`.
3. Add an entry `controller/web/src/entries/about.ts`.
4. `bun run build` — the new `about-<hash>.js` is picked up automatically.
