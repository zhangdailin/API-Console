# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.6
FROM golang:${GO_VERSION}-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X orchids-api/internal/buildinfo.Version=${VERSION} -X orchids-api/internal/buildinfo.Commit=${COMMIT} -X orchids-api/internal/buildinfo.Date=${BUILD_DATE} -X orchids-api/internal/buildinfo.BuildType=docker" \
    -o /out/orchids-server ./cmd/server

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -g 10001 app \
    && adduser -D -H -u 10001 -G app app \
    && mkdir -p /app/data \
    && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/orchids-server /usr/local/bin/orchids-server
USER 10001:10001
EXPOSE 3002
ENTRYPOINT ["/usr/local/bin/orchids-server"]
CMD ["-config", "/app/config.json"]
