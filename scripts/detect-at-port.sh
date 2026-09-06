#!/bin/sh
# =============================================================================
# detect-at-port.sh — 自动识别 QDC507 的 AT 串口（ttyUSB 几号）
#
# 原理：向每个 /dev/ttyUSB* 发送 AT 并等待 "OK" 回包。
#      能被 AT 指令应答的端口就是 AT 口（通常也是唯一一个）。
#
# 用法:
#   ./scripts/detect-at-port.sh            # 打印 AT 口路径（如 /dev/ttyUSB2）
#   ./scripts/detect-at-port.sh --list     # 打印全部 USB 口
#
# POSIX sh（dash/busybox）兼容；无需 root。
# =============================================================================
set -u

LIST_ONLY=0
[ "${1:-}" = "--list" ] && LIST_ONLY=1

if [ "$LIST_ONLY" = "1" ]; then
  echo "== USB 串口清单 =="
  for p in /dev/ttyUSB*; do
    [ -e "$p" ] || continue
    echo "  $p"
  done
  echo "== by-id 稳定路径 =="
  ls -l /dev/serial/by-id/ 2>/dev/null | grep -E "usb-.*if0[0-9]-port0" | awk '{print "  /dev/serial/by-id/"$9" -> "$11}'
  exit 0
fi

# 逐口探测 AT 应答（只读测试，不写配置）
for p in /dev/ttyUSB*; do
  [ -e "$p" ] || continue
  stty -F "$p" 9600 cs8 -cstopb -parenb raw -echo 2>/dev/null
  reply=""
  try=1
  while [ "$try" -le 2 ]; do
    reply=$(timeout 1.5 sh -c "printf 'AT\r' > '$p'; cat '$p'" 2>/dev/null)
    [ -n "$reply" ] && break
    try=$((try + 1))
  done
  if printf '%s' "$reply" | grep -q "OK"; then
    echo "$p"
    if [ -z "${QUIET:-}" ]; then
      model=$(timeout 1.5 sh -c "printf 'AT+CGMM\r' > '$p'; cat '$p'" 2>/dev/null)
      echo "  → $(printf '%s' "$model" | tr -d '\r' | grep -vE '^AT|OK|^$' | head -n1)" >&2
    fi
    exit 0
  fi
done

echo "❌ 未找到可应答 AT 的串口。检查: lsusb 是否出现 2C7C:0125; 模块是否上电; 是否被其他进程占用" >&2
exit 1