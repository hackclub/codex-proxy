FROM golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /codex-proxy ./cmd/server

FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
    && addgroup -S codexproxy \
    && adduser -S -G codexproxy codexproxy
COPY --from=build /codex-proxy /usr/local/bin/codex-proxy
USER codexproxy
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/codex-proxy"]

