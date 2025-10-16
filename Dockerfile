FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -ldflags='-w -s' -o goreilly ./cmd/server

FROM alpine:latest

RUN apk add --no-cache ca-certificates wget python3 py3-pip && \
    wget -q https://github.com/kovidgoyal/calibre/releases/download/v7.18.0/calibre-7.18.0-x86_64.txz && \
    tar xf calibre-7.18.0-x86_64.txz -C /opt && \
    rm calibre-7.18.0-x86_64.txz && \
    ln -s /opt/calibre/ebook-convert /usr/bin/ebook-convert
WORKDIR /app

COPY --from=builder /build/goreilly .

RUN mkdir -p Books Converted

ENV PORT=3000 GIN_MODE=release

EXPOSE 3000

CMD ["./goreilly"]
