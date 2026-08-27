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
| `cmd/ironhive-controller/` | Controller binary: flags (`-listen` / `IRONHIVE_LISTEN`, default `:8080`), graceful shutdown |
| `cmd/ironhive-runtime/` | Runtime binary: agent running inside managed containers |
| `controller/` | Controller package: HTTP server, views, static assets |
| `controller/server.go` | `http.ServeMux` with method+path patterns, security headers, page handlers |
| `controller/web_tmpl.go` | `//go:embed web/view/*.html`, template funcs `jsAsset` / `cssAsset` |
| `controller/web_static.go` | `//go:embed all:web/dist`, `<entry>-<hash>.<ext>` matching, `/static/` handler |
| `controller/web/build.ts` | Bun build: bundles every entry in `src/entries/` into hashed IIFEs in `dist/` |
| `controller/web/src/entries/` | One file per bundle: page TS entries plus `main.css` (Tailwind v4) |
| `controller/web/view/` | Go templates; `base.html` defines shared `head` / `nav` blocks |
| `runtime/` | Runtime package: agent logic for managed containers |

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
