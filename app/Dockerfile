# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
# Set GO_TAGS=chaos only for deliberately broken demo releases.
ARG GO_TAGS=""
# nginx talks HTTP/1.1 to the app, so the HTTP/2 server code is left out.
RUN CGO_ENABLED=0 go build -trimpath -tags "nethttpomithttp2 $GO_TAGS" \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o /out/parkd ./cmd/parkd

FROM alpine:3.22
RUN addgroup -S -g 10001 parkd && adduser -S -D -H -u 10001 -G parkd parkd
COPY --from=build /out/parkd /usr/local/bin/parkd

# Empty in regular builds; only read by binaries built with the chaos tag.
ARG CHAOS_MODE=""
ENV CHAOS_MODE=$CHAOS_MODE

USER parkd
EXPOSE 8080
HEALTHCHECK --interval=5s --timeout=3s --start-period=10s --retries=3 \
    CMD ["/usr/local/bin/parkd", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/parkd"]
