# syntax=docker/dockerfile:1
# MSP server image. Build context is the repository root:
#   docker build -f deploy/Dockerfile -t msp-server:dev .
#
# Three stages:
#   web   - bundles the Preact dashboard with esbuild (needs Node, never needed at runtime)
#   build - compiles the static server binary and the desktop agent matrix (CGO off)
#   final - alpine, non-root, the server plus /agents served at /dl/
#
# The image is the only place where Node and Go are needed; the runtime stage has neither.

# ---- stage 1: dashboard bundle ---------------------------------------------------------
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
# Copy only the inputs of build.mjs. A host checkout may carry web/node_modules (with an
# esbuild binary for another platform) and web/dist/app.js; neither must leak in here.
COPY web/build.mjs web/tsconfig.json ./
COPY web/src/ ./src/
RUN npm run build

# ---- stage 2: server + agent binaries ----------------------------------------------------
FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The bundle produced above replaces whatever web/dist held in the build context; the
# committed web/dist/index.html stays and go:embed picks up app.js + app.css.
COPY --from=web /src/web/dist/ web/dist/
RUN V=$(cat VERSION) \
 && go build -trimpath -ldflags "-s -w -X main.version=$V" -o /out/msp-server ./cmd/msp-server \
 && for t in linux/amd64 linux/arm64 windows/amd64; do \
      ext=""; [ "${t%/*}" = windows ] && ext=.exe; \
      GOOS=${t%/*} GOARCH=${t#*/} go build -trimpath -ldflags "-s -w -X main.version=$V" \
        -o "/out/agents/msp-agent-${t%/*}-${t#*/}${ext}" ./cmd/msp-agent || exit 1; \
    done \
 && cp deploy/install.sh deploy/install.ps1 /out/agents/ \
 && (cd /out/agents && sha256sum * > SHA256SUMS)

# ---- stage 3: runtime --------------------------------------------------------------------
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 msp \
 && mkdir -p /data \
 && chown msp:msp /data
COPY --from=build /out/msp-server /usr/local/bin/msp-server
COPY --from=build /out/agents /agents
VOLUME /data
ENV MSP_DB=/data/msp.db \
    MSP_LISTEN=:8080 \
    MSP_AGENT_DIR=/agents
USER msp
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["msp-server"]
CMD ["serve"]
