# CLIProxyAPI Docker镜像构建文件
# 原生编译：GitHub Actions 按平台矩阵使用原生 runner（amd64/arm64），
# 因此直接用 go build 编译（不再用 tonistiigi/xx + clang 交叉编译，
# 避免 xx/clang 构建出的 host 二进制 dlopen 动态插件时崩溃）
# syntax=docker/dockerfile:1.4

# 构建阶段 - 使用 BUILDPLATFORM（= 目标平台，原生）
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# 原生构建工具链
RUN apk add --no-cache git ca-certificates tzdata gcc musl-dev

WORKDIR /app

ENV GOPROXY=https://proxy.golang.org,direct

COPY go.mod go.sum ./

RUN --mount=type=cache,target=/root/.cache/go-mod \
    go mod download

COPY . .

# 启用 cgo：CPA 动态库插件（dlopen .so）依赖 cgo
ENV CGO_ENABLED=1
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/.cache/go-mod \
    go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w \
      -X 'main.Version=${VERSION}' \
      -X 'main.Commit=${COMMIT}' \
      -X 'main.BuildDate=${BUILD_DATE}'" \
    -o ./CLIProxyAPI ./cmd/server/

# 运行阶段
FROM alpine:3.23

RUN apk add --no-cache tzdata ca-certificates

RUN mkdir /CLIProxyAPI

COPY --from=builder /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI

COPY config.example.yaml /CLIProxyAPI/config.example.yaml

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPI"]
