FROM golang:1.26.1-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY cmd/mechbot ./cmd/mechbot
COPY internal/bot ./internal/bot
COPY internal/proto ./internal/proto
COPY internal/world ./internal/world
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=mod GOTOOLCHAIN=local
RUN go build -trimpath -ldflags="-s -w" -o /out/mechbot ./cmd/mechbot

FROM scratch
COPY --from=build /out/mechbot /mechbot
USER 65534:65534
ENTRYPOINT ["/mechbot"]
