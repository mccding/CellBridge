#!/bin/sh
# =============================================================================
# init-module.sh — QDC507 模块一键初始化（adb 解锁 + 开启 adb+UAC）
#
# 关键顺序（按模块固件要求，顺序不可颠倒）:
#   1. 自动识别 AT 串口（复用 detect-at-port.sh）
#   2. 查询 +QADBKEY? 检查 adb 是否已解锁
#      - 已解锁 → 无需解锁，继续
#      - 未解锁 → 需要 15 字符 key（AT+QADBKEY="<key>" 解锁）
#   3. 查询当前 usbcfg（已开则跳过）
#   4. 写入 usbcfg 全开（Dload,AT,Modem,NMEA,Diag,ADB,UAC = 1,1,1,1,1,1,1）
#   5. 读回验证
#   6. AT+CFUN=1,1 重启射频子系统使生效
#   7. 提示等待重枚举 + 验证命令
#
# 用法:
#   ./scripts/init-module.sh                       # 一键（自动识别 AT 口）
#   ./scripts/init-module.sh /dev/ttyUSB2          # 指定 AT 口
#   ./scripts/init-module.sh --check               # 只查状态不写
#   ADB_KEY=abcdefghijklmn ./scripts/init-module.sh  # 提供 key 自动解锁
#
# 说明:
#   - 多数模块出厂 adb 已解锁（QADBKEY 返回 OK 即可写 usbcfg）
#   - 少数模块 adb 被锁定（QADBKEY 返回 challenge），必须先解锁，
#     否则 usbcfg 写入会被拒绝。key 是厂商签发的 15 字符 MD5-crypt
#     格式，模块绑定，向你的模块来源方索取。
#   - 本脚本流程与 MaVo enable-adb-config 一致：查状态 → 幂等跳过
#     → 写 usbcfg → 读回 → 重启。
# =============================================================================
set -u

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ADB_KEY="${ADB_KEY:-}"
AT_PORT=""
ACTION="init"
[ "${1:-}" = "--check" ] && ACTION="check" && shift

if [ "${1:-}" != "" ]; then
  AT_PORT="$1"
else
  echo "==> 自动识别 AT 串口..." >&2
  AT_PORT=$(sh "$SCRIPT_DIR/detect-at-port.sh" 2>/dev/null)
  if [ -z "$AT_PORT" ]; then
    echo "❌ 未识别到 AT 口。请确认模块已插入 NAS，或手动指定: $0 /dev/ttyUSBx" >&2
    exit 1
  fi
fi
[ -e "$AT_PORT" ] || { echo "❌ $AT_PORT 不存在" >&2; exit 1; }
echo "    AT 口: $AT_PORT" >&2

at_cmd() {
  timeout 3 sh -c "printf '${1}\r' > '${AT_PORT}'; cat '${AT_PORT}'" 2>/dev/null
}

# --------------------------------------------------------------- 解锁 adb root
# 高通 QADBKEY 挑战-应答：固定 secret + MD5-crypt(challenge) 派生解锁密码。
# 出处：Quectel 公开实现的 QADBKEYSecret = "SH_adb_quectel"（见 dji-voice-go 等
# 开源项目）。解锁持久生效（跨重启），模块只需解锁一次。
# 注意：本密码是公开算法产物，不是秘密；厂商用同一算法签发。
QADBKEY_SECRET="SH_adb_quectel"

md5crypt_password() {
  # glibc MD5-crypt（$1$salt$hash），返回 hash 部分。
  # 算法与 dji-voice-go/qadbkey.go 完全一致（低位优先 encode）。
  python3 - "$QADBKEY_SECRET" "$1" <<'PYEOF'
import sys, hashlib

def md5crypt(password, salt):
    alt = hashlib.md5((password + salt + password).encode()).digest()
    d = hashlib.md5((password + "$1$" + salt).encode()).digest()
    i = len(alt)
    while i > 0:
        d = hashlib.md5(d + alt[:min(16, i)]).digest()
        i -= 16
    i = len(password)
    while i > 0:
        d = hashlib.md5(d + (b"\x00" if i & 1 else password[:1].encode())).digest()
        i >>= 1
    for i in range(1000):
        inp = (password if i & 1 else d)
        if isinstance(inp, bytes) and len(inp) == 16:
            inp = d
        inp = inp if isinstance(inp, bytes) else inp.encode()
        if i % 3 != 0:
            inp += salt.encode()
        if i % 7 != 0:
            inp += password.encode()
        if i & 1:
            inp += d
        else:
            inp += password.encode()
        d = hashlib.md5(inp).digest()
    alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
    def enc(b1, b2, b3, n):
        w = (b1 << 16) | (b2 << 8) | b3
        out = ""
        for i in range(n):
            out += alphabet[(w >> (6 * i)) & 0x3f]   # 低位优先，与 Go 版 w >>= 6 一致
        return out
    b = d
    return (enc(b[0], b[6], b[12], 4) + enc(b[1], b[7], b[13], 4) +
            enc(b[2], b[8], b[14], 4) + enc(b[3], b[9], b[15], 4) +
            enc(b[4], b[10], b[5], 4) + enc(0, 0, b[11], 2))

print(md5crypt(sys.argv[1], sys.argv[2]))
PYEOF
}

echo "==> 检查 adb 解锁状态 (AT+QADBKEY?)..." >&2
KEY_RESP=$(at_cmd 'AT+QADBKEY?')
KEY_CLEAN=$(printf '%s' "$KEY_RESP" | tr -d '\r')
echo "    响应: $(echo "$KEY_CLEAN" | grep -iE 'QADBKEY|error' | head -n1)" >&2

# QADBKEY 挑战值（+QADBKEY: <8位数字>）
CHALLENGE=$(printf '%s' "$KEY_CLEAN" | grep -i '+QADBKEY:' | head -n1 | sed 's/.*QADBKEY:[[:space:]]*//' | tr -d '"')

if [ -n "$CHALLENGE" ]; then
  # 有挑战值 → 尝试解锁（固定 secret 派生密码）；--check 模式只读不发
  if [ "$ACTION" = "check" ]; then
    echo "    （challenge 存在; 执行 $0 将自动解锁 adb root）" >&2
  else
    PASSWORD=$(md5crypt_password "$CHALLENGE" 2>/dev/null)
    if [ -n "$PASSWORD" ]; then
      echo "==> 解锁 adb root (挑战 $CHALLENGE → MD5-crypt 密码)..." >&2
      UNLOCK_RESP=$(at_cmd "AT+QADBKEY=\"$PASSWORD\"")
      if printf '%s' "$UNLOCK_RESP" | grep -qi "OK"; then
        echo "    ✅ adb root 已解锁（持久生效）" >&2
      else
        echo "    ⚠️  解锁指令未获 OK（模块可能已解锁，拒绝重复解锁——正常）" >&2
      fi
    fi
  fi
else
  echo "    （无 QADBKEY 挑战 = 固件无此命令）" >&2
fi

# --------------------------------------------------------------- 查 usbcfg
echo "==> 查询当前 usbcfg..." >&2
CURRENT=$(at_cmd 'AT+QCFG="usbcfg"')
echo "    当前: $(printf '%s' "$CURRENT" | tr -d '\r' | grep -i 'usbcfg' | head -n1)" >&2

if printf '%s' "$CURRENT" | grep -qiE 'usbcfg.*1,1,1,1,1,1,1'; then
  echo "✅ usbcfg 已是全开状态（adb+UAC 已启用），无需修改。" >&2
  if [ "$ACTION" = "check" ]; then exit 0; fi
  echo "==> 无需重启。直接验证: adb devices -l" >&2
  exit 0
fi
if [ "$ACTION" = "check" ]; then
  echo "ℹ️  usbcfg 未全开。执行 $0 将写入全开配置并重启。" >&2
  exit 0
fi

# --------------------------------------------------------------- 写 usbcfg
echo "==> 写入 usbcfg 全开（adb+UAC）..." >&2
WRITE_RESP=$(at_cmd 'AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1')
if ! printf '%s' "$WRITE_RESP" | grep -q "OK"; then
  sleep 1
  WRITE_RESP=$(at_cmd 'AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1')
fi
if ! printf '%s' "$WRITE_RESP" | grep -q "OK"; then
  echo "❌ usbcfg 写入未获确认。响应: $(printf '%s' "$WRITE_RESP" | tr -d '\r' | tail -n2)" >&2
  echo "   若提示 lock/challenge: adb 仍被锁定，需先解锁（见上 QADBKEY 步骤）" >&2
  exit 1
fi
echo "    ✅ 写入确认" >&2

echo "==> 读回验证..." >&2
VERIFY=$(at_cmd 'AT+QCFG="usbcfg"')
if printf '%s' "$VERIFY" | grep -qiE 'usbcfg.*1,1,1,1,1,1,1'; then
  echo "    ✅ 读回一致: $(printf '%s' "$VERIFY" | tr -d '\r' | grep -i 'usbcfg' | head -n1)" >&2
else
  echo "⚠️  读回未匹配（部分模块写入后立即断开 USB 属正常）" >&2
fi

# --------------------------------------------------------------- 重启生效
echo "==> 重启射频子系统使生效 (AT+CFUN=1,1)..." >&2
CFUN_RESP=$(at_cmd 'AT+CFUN=1,1')
printf '%s' "$CFUN_RESP" | grep -q "OK" && echo "    ✅ 重启指令已接受" >&2 || echo "    ⚠️ CFUN 响应超时（模块重启中，正常）" >&2

echo ""
echo "======================================================================" >&2
echo " 初始化完成！模块 USB 正在重枚举（约 15 秒）。" >&2
echo " 等待后验证:" >&2
echo "   adb devices -l            # 应出现 QDC507 设备" >&2
echo "   cat /proc/asound/cards    # 应出现 BAIWANG (USB Audio) -> hw:0,0" >&2
echo "======================================================================" >&2