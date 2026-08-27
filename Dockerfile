FROM oven/bun:1 AS static
# Mirror the repo layout: main.css's tailwind @source glob ("../../../*.go")
# scans the top-level Go files, so the Go files must sit next to web/ —
# resolving past them (to the container root /) walks the whole filesystem
# (/proc, /usr, ...) and the build hangs forever.
WORKDIR /repo/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
COPY *.go /repo/
RUN bun run build

FROM golang:1.27 AS builder
ENV CGO_ENABLED=0
WORKDIR /go/src/app
COPY . .
COPY --from=static /repo/web/dist web/dist
RUN go build -trimpath -ldflags="-s -w" -o /ironhive

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
	tini \
	ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
COPY --from=builder /ironhive /ironhive
ENV IRONHIVE_LISTEN=:8080
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["/ironhive"]
