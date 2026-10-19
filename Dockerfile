# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
# 国内构建：直连 proxy.golang.org 不通，改用 goproxy.cn 镜像；GOSUMDB=off 避免
# sum.golang.org 校验超时。海外 / CI 如需官方源，删掉下面两行即可。
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/omnigate-panel ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /app/data \
 && chown -R app:app /app
WORKDIR /app
COPY --from=build /out/omnigate-panel /app/omnigate-panel
COPY config.example.json omnigate.example.json /app/
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/omnigate-panel", "-config", "/app/config.json"]
