#!/usr/bin/env bash
# =============================================================================
# CellBridge 一键部署脚本（build + upload + replace + restart）
#
# 用法:
#   ./scripts/deploy.sh                     # NAS 默认 nasanysim
#   NAS_HOST=my-nas ./scripts/deploy.sh     # 指定 NAS
#   ./scripts/deploy.sh --no-build          # 跳过编译, 只部署已有产物
#
# 前置: tailscale ssh root@<NAS_HOST> 可用
# 安全: 部署前自动备份当前运行二进制为 .pre-deploy-<时间戳>
# =============================================================================
set -euo pipefail

cd "$(dirname "$0")/.."

NAS_HOST="${NAS_HOST:-nasanysim}"
REMOTE_BIN=/mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway
LOCAL_BIN=artifacts/cellbridge-gateway-linux-arm64

# 1. 编译（默认本机架构；NAS 是 arm64 用 arm64，x86 传 GOARCH=amd64）
GOARCH="${GOARCH:-arm64}"
if [[ "${1:-}" != "--no-build" ]]; then
  echo "==> [1/4] 编译 linux/${GOARCH} ..."
  (cd gateway && GOOS=linux GOARCH="${GOARCH}" CGO_ENABLED=0 \
    go build -trimpath -ldflags='-s -w' \
    -o "../${LOCAL_BIN}" ./cmd/cellbridge-gateway)
else
  echo "==> [1/4] 跳过编译（--no-build）"
fi

echo "==> [2/4] 备份 NAS 当前二进制 ..."
tailscale ssh "root@${NAS_HOST}" "cp ${REMOTE_BIN} ${REMOTE_BIN}.pre-deploy-\$(date +%s) 2>/dev/null || true"

echo "==> [3/4] 上传并替换 ..."
scp "${LOCAL_BIN}" "root@${NAS_HOST}:/tmp/cellbridge-gateway.new"
tailscale ssh "root@${NAS_HOST}" "mv /tmp/cellbridge-gateway.new ${REMOTE_BIN} && chmod +x ${REMOTE_BIN}"

echo "==> [4/4] 重启并验证 ..."
tailscale ssh "root@${NAS_HOST}" "systemctl restart cellbridge-gateway && sleep 4 && systemctl is-active cellbridge-gateway"

echo "==> ✅ 部署完成"