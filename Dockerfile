FROM golang:1.25 AS builder

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -ldflags='-w -s' -o goreilly ./cmd/server

FROM ubuntu:latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates wget python3 python3-pip xz-utils libegl1 libopengl0 libxcb-cursor0 libfreetype6 && \
    wget -nv -O- https://download.calibre-ebook.com/linux-installer.sh | sh /dev/stdin install_dir=/opt/calibre-bin isolated=y && \
    ln -s /opt/calibre-bin/ebook-convert /usr/bin/ebook-convert && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /build/goreilly .

ENV PORT=3000 GIN_MODE=release

EXPOSE 3000

CMD ["./goreilly"]