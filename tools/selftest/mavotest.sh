#!/system/bin/sh
nohup /run/maccellular-call/mavo-pcm-bridge.armv7 --voice-route-session --verbose </dev/null >> /run/celldock-voice-route.log 2>&1 &
pid=$!
starttime=$(cut -d ' ' -f 22 /proc/$pid/stat 2>/dev/null)
echo "ARGS pid=$pid start=$starttime"
printf '%s %s\n' "$pid" "$starttime" > /run/celldock-voice-route.pid
ls -la /run/celldock-voice-route.pid
cat /run/celldock-voice-route.pid
