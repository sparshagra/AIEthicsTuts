# The match runs it with no network, a read-only root (writable /tmp), 1 CPU, 512 MB, and the map at /arena.
FROM golang:1.23-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY pilot.go strategy.go terrain.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /pilot .

FROM scratch
COPY --from=build /pilot /pilot
COPY arena /arena
ENTRYPOINT ["/pilot"]
