#!/bin/sh
# readycheck.sh — 模块侧语音路由 READY 逐项检查（在 NAS 上运行，依赖 adb）
# 用法: sh readycheck.sh
# 每项输出 [OK] 或 [FAIL]，全部 OK = 路由会话健全。

set -u
FAIL=0

echo "--- A. pid 文件 ---"
if [ -s /run/celldock-voice-route.pid ]; then
  read pid st < /run/celldock-voice-route.pid
  echo "file: $pid $st"
else
  echo "[FAIL] /run/celldock-voice-route.pid 缺失或为空"
  FAIL=1
  exit 1
fi

echo "--- B. mavo 进程 --"
if adb shell "ps | grep mavo-pcm-bridge | grep -v grep" >/dev/null 2>&1; then
  echo "process alive (pid file says $pid)"
else
  echo "[FAIL] mavo-pcm-bridge 不在运行"
  FAIL=1
fi

echo "--- C. 路由日志 --"
if adb shell "grep -q 'VoLTE route session active on hw:0,4' /run/celldock-voice-route.log" 2>/dev/null; then
  echo "log line present"
else
  echo "[FAIL] 日志缺少 VoLTE route session active"
  FAIL=1
fi

echo "--- D. audio_enable --"
v=$(adb shell "cat /sys/class/android_usb/f_audio/audio_enable" 2>/dev/null | tr -d '\r')
if [ "$v" = "1" ]; then
  echo "audio_enable=$v"
else
  echo "[FAIL] audio_enable=$v (期望 1)"
  FAIL=1
fi

echo "--- E. hw:0,4 playback RUNNING --"
if adb shell "grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status" 2>/dev/null; then
  echo "pcm4p RUNNING"
else
  echo "[FAIL] pcm4p 非 RUNNING"
  FAIL=1
fi

echo "--- F. hw:0,4 capture RUNNING --"
if adb shell "grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status" 2>/dev/null; then
  echo "pcm4c RUNNING"
else
  echo "[FAIL] pcm4c 非 RUNNING"
  FAIL=1
fi

echo
if [ "$FAIL" = "0" ]; then
  echo "== READY =="
else
  echo "== NOT READY =="
fi
exit $FAIL