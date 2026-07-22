# ---- build ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/belay-sh/belay/internal/version.Version=${VERSION}" \
    -o /belay ./cmd/belay

# ---- run ----
FROM alpine:3.20
# docker CLI + compose plugin: belay drives `docker compose` against the mounted socket.
# git: for the git-optional "commit each tag bump" mode.
RUN apk add --no-cache docker-cli docker-cli-compose git ca-certificates tzdata
COPY --from=build /belay /usr/local/bin/belay
ENTRYPOINT ["belay"]
CMD ["server"]
