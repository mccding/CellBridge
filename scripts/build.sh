#!/usr/bin/env bash
# =============================================================================
# CellBridge Gateway 一键编译脚本（arm64 + x86_64 双架构）
#
# 用法:
#   ./scripts/build.sh                # 仅编译二进制到 artifacts/
#   ./scripts/build.sh docker         # 编译二进制 + 构建 Docker 镜像（本机架构）
#   ./scripts/build.sh docker-all     # 编译二进制 + buildx 双平台镜像（需 docker buildx）
#
# 产物:
#   artifacts/cellbridge-gateway-linux-arm64
#   artifacts/cellbridge-gateway-linux-amd64
#   artifacts/SHA256SUMS.txt
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p artifacts

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=${VERSION}"

echo "==> 编译 cellbridge-gateway（version=${VERSION}）"

# arm64（飞牛/树莓派/NAS 常见架构）
echo "==> linux/arm64 ..."
(cd gateway && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="${LDFLAGS}" \
  -o ../artifacts/cellbridge-gateway-linux-arm64 ./cmd/cellbridge-gateway)

# amd64（x86 服务器 / Docker 默认）
echo "==> linux/amd64 ..."
(cd gateway && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="${LDFLAGS}" \
  -o ../artifacts/cellbridge-gateway-linux-amd64 ./cmd/cellbridge-gateway)

echo "==> SHA256SUMS ..."
(cd artifacts && sha256sum cellbridge-gateway-linux-* > SHA256SUMS.txt)
chmod +x artifacts/cellbridge-gateway-linux-*
ls -lh artifacts/cellbridge-gateway-linux-*

if [[ "${1:-}" == "docker" ]]; then
  echo "==> Docker 镜像（本机架构）..."
  docker build --build-arg TARGETARCH=$(go env GOARCH || echo amd64) \
    -f gateway/Dockerfile -t cellbridge-gateway:${VERSION} .
  echo "==> 完成: docker images | grep cellbridge-gateway"
fi

if [[ "${1:-}" == "docker-all" ]]; then
  echo "==> Docker 双平台镜像（arm64+amd64，推送到 GHCR 请自行改 repo 名）..."
  docker buildx build --platform linux/arm64,linux/amd64 \
    -f gateway/Dockerfile \
    -t cellbridge-gateway:${VERSION} \
    -t ghcr.io/${GITHUB_REPO:-yourname/cellbridge}:${VERSION} \
    --push=false .
  echo "==> 完成；如需推送: --push=true"
fi

echo "==> 全部完成 ✅"