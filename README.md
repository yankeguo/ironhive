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
| `cmd/ironhive-controller/` | Controller binary: flags (`-listen` / `IHC_LISTEN`, default `:8080`), graceful shutdown |
| `cmd/ironhive-runtime/` | Runtime binary: agent running inside managed containers |
| `controller/` | Controller package: HTTP server, views, static assets |
| `controller/server.go` | `http.ServeMux` with method+path patterns, security headers, page handlers |
| `controller/web_tmpl.go` | `//go:embed web/view/*.html`, template funcs `jsAsset` / `cssAsset` |
| `controller/web_static.go` | `//go:embed all:web/dist`, `<entry>-<hash>.<ext>` matching, `/static/` handler |
| `controller/web/build.ts` | Bun build: bundles every entry in `src/entries/` into hashed IIFEs in `dist/` |
| `controller/web/src/entries/` | One file per bundle: page TS entries plus `main.css` (Tailwind v4) |
| `controller/web/view/` | Go templates; `base.html` defines shared `head` / `nav` blocks |
| `runtime/` | Runtime package: agent logic for managed containers, PID 1 zombie reaping |

## ironhive-runtime

`ironhive-runtime` is the agent running as the main process inside managed containers. Flags: `-listen` / `IHR_LISTEN` (default `:8080`).

As **PID 1** it reaps orphaned zombies itself (SIGCHLD-driven `wait4(-1)`), so the image needs no tini — `Dockerfile.runtime` uses the binary directly as `ENTRYPOINT`.

### API

| Endpoint | Description |
|---|---|
| `GET /healthz` | Liveness probe, returns `OK` |
| `GET /v1/file?path=` | Download a file as an attachment (`Range` supported). `path` may be absolute, or relative to the process working directory |
| `PUT /v1/file?path=` | Upload a file **atomically**: the body lands in a temp file in the target directory, then is renamed over the target. Optional `chmod` (zero-prefixed octal, e.g. `0644`) and `chown` (`user:group`; names or numeric ids, either side omittable, e.g. `user`, `:group`, `1000:1000`) |
| `PUT /v1/tar?path=` | Extract an uncompressed tar stream into `path` (created if missing). Regular files and directories with preserved modes and mtimes; absolute entry names, `..` traversal and other entry types are rejected |
| `POST /v1/shell` | Run the form field `command` via bash and stream output as server-sent events (see below) |

File operations on the same absolute path are serialized with a per-path mutex.

### Shell sessions

`POST /v1/shell` runs each command embedded in a bash wrapper that restores the previous call's environment and working directory from on-disk snapshots (`$TMPDIR/ironhive-shell/{env,pwd}`) and saves them again on exit via `trap` — so `cd` and `export` carry over between calls without a long-lived shell. The initial working directory is the process cwd. Calls are serialized because they share the state files.

The response is `text/event-stream`; `data` is a JSON-encoded string:

```
event: stdout
data: "hello"

event: stderr
data: "something failed"

event: exit
data: "0"
```

One `stdout`/`stderr` event per output line, then a final `exit` event with the exit code.

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
go build ./cmd/ironhive-controller ./cmd/ironhive-runtime
```

`controller/web/dist` is git-ignored (only `.gitkeep` is committed), so always run the frontend build before `go build` — in Docker, do it in an `oven/bun` stage.

## Release

`.github/workflows/release.yml` builds and pushes `ghcr.io/<owner>/<repo>` via the multi-stage `Dockerfile.controller` and `Dockerfile.runtime`, with tags prefixed by component:

- push `main` → `controller-latest` / `runtime-latest` and `controller-latest-<short_sha>` / `runtime-latest-<short_sha>`
- push a git tag → `controller-<tag>` / `runtime-<tag>`

Note: the bun stage mirrors the repo layout (`WORKDIR /repo/controller/web`, `COPY controller/*.go /repo/controller/`) because `main.css`'s Tailwind `@source "../../../*.go"` resolves relative to the CSS file — without the Go files next to `web/`, the glob lands on the container root and the build hangs scanning the whole filesystem.

## Adding a page

1. Add a route in `controller/server.go`, e.g. `mux.HandleFunc("GET /about", s.handleAbout)`.
2. Add a view `controller/web/view/about.html` with `{{template "head" .}}` and `<script src="{{jsAsset "about"}}" defer></script>`.
3. Add an entry `controller/web/src/entries/about.ts`.
4. `bun run build` — the new `about-<hash>.js` is picked up automatically.
