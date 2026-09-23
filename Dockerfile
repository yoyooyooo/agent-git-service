# ---- Build stage ----
# Supported attributable builds invoke this Dockerfile only through
# scripts/build-exact-source.sh, so COPY reads the verified commit archive
# candidate rather than the mutable operator checkout.
FROM golang:1.25-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG GIT_SHA
ARG GIT_TREE
RUN case "${GIT_SHA}" in (*[!0-9a-f]*|'') echo "GIT_SHA must be lowercase 40-hex" >&2; exit 1;; esac \
    && case "${GIT_TREE}" in (*[!0-9a-f]*|'') echo "GIT_TREE must be lowercase 40-hex" >&2; exit 1;; esac \
    && test "${#GIT_SHA}" -eq 40 \
    && test "${#GIT_TREE}" -eq 40 \
    && test "$(cat .ags-source-revision)" = "${GIT_SHA}" \
    && test "$(cat .ags-source-tree)" = "${GIT_TREE}" \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X github.com/ngaut/agent-git-service/server.gitSHA=${GIT_SHA}" -o gh-server ./cmd/gh-server

# ---- Runtime stage ----
FROM alpine:3.21
RUN apk add --no-cache git git-daemon \
    && test -x "$(git --exec-path)/git-http-backend"

RUN adduser -D -h /data appuser
USER appuser

COPY --from=builder /app/gh-server /usr/local/bin/gh-server

ENV GIT_REPO_DIR=/data/repos
ENV LISTEN_MODE=production
RUN mkdir -p /data/repos

EXPOSE 8080

ENTRYPOINT ["gh-server"]
