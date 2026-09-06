<div align="center">
# CellBridge
**Turn any QDC507 4G module + SIM card into a personal cellular gateway for your iPhone.**
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://img.shields.io/badge/CI-GitHub%20Actions-blue)](.github/workflows/build.yml)
**[简体中文](README.md) (default)**
</div>
---

# CellBridge: QDC507 4G Module + NAS Gateway + YakPhone, End-to-End (Voice / SMS / Inbound Calls)

> Goal: let anyone follow this document and reproduce, from scratch, a complete system where a SIM card sits on a QDC507 module and the iPhone makes VoLTE calls and sends/receives SMS through YakPhone.
> Every command in this document is in its final form, **verified end-to-end on real hardware on 2026-09-06**. The system delivers: audio on outbound calls, inbound ringing + push, SMS send/receive, and hangup-then-redial without 500s.

---

## Table of Contents

1. [Overall Topology](#1-overall-topology)
2. [Hardware Preparation](#2-hardware-preparation)
3. [Tailscale Networking](#3-tailscale-networking)
4. [Building the Gateway for arm64 and x86](#4-building-the-gateway-for-arm64-and-x86)
5. [Deploying to the NAS](#5-deploying-to-the-nas)
6. [config.yaml and SIP Parameters](#6-configyaml-and-sip-parameters)
7. [YakPhone Client Configuration](#7-yakphone-client-configuration)
8. [Voice Path](#8-voice-path)
9. [SMS Path](#9-sms-path)
10. [PushKit Push](#10-pushkit-push)
11. [Self-Test Scripts](#11-self-test-scripts)
12. [Eight-Layer Troubleshooting Methodology (§36)](#12-eight-layer-troubleshooting-methodology-36)
13. [Known Pitfalls and Workarounds](#13-known-pitfalls-and-workarounds)
14. [Security and Call-Charge Protection](#14-security-and-call-charge-protection)
15. [Open-Source Information and Legal](#15-open-source-information-and-legal)

---

## 1. Overall Topology

```text
                ┌──────────────────────────────┐
                │           iPhone             │
                │      YakPhone (baresip)      │
                │  SIP 5060/UDP + RTP PCMU     │
                │  PushKit (call/SMS wake-up)  │
                └───────────┬──────────────────┘
                            │ Tailscale (direct tailnet, no NAT)
                            ▼
                ┌──────────────────────────────┐
                │       NAS (fnOS/ARM64)       │
                │   cellbridge-gateway process │
                │   ├── SIP server 0.0.0.0:5060│
                │   ├── RTP bridge (PCMU/8000) │
                │   ├── SMS engine (AT+CMGS)   │
                │   └── voice-route mgmt (mavo)│
                └───────────┬──────────────────┘
                            │ USB (UAC: hw:0,0 + AT: ttyUSB2)
                            ▼
                ┌──────────────────────────────┐
                │  QDC507 4G module (DJI Gen-1)│
                │   ├── mavo-pcm-bridge (voice)│
                │   ├── qdc507_aprv3/voice.ko  │
                │   └── SIM card (any carrier) │
                └───────────┬──────────────────┘
                            │ Cellular network (VoLTE)
                            ▼
                    10010 customer service / any number
```

**Architecture essentials (final, don't change):**
- **The NAS is the SIP server** (not an HTTP API); YakPhone registers directly to NAS:5060 with no intermediary HTTP hop.
- **Voice = SIP/RTP (PCMU 8000Hz; phase 1 does not enable TLS/SRTP/STUN/TURN)**; direct connection inside the tailnet means no NAT, so `turn:` stays empty everywhere.
- **VoLTE voice routing**: on the module, `mavo-pcm-bridge --voice-route-session` opens `hw:0,4` (VoLTE PCM), and the USB UAC (`f_audio`) streams PCM to the NAS's `hw:0,0` (arecord/aplay).
- **SMS**: YakPhone sends via **SIP MESSAGE** to the NAS → the NAS uses `AT+CMGS` (text mode for short codes / PDU for long numbers) over ttyUSB2 to reach the module. Incoming SMS = module `+CMTI` URC → NAS stores it → `push.yakteam.com` pushes to wake YakPhone.
- **PushKit**: the NAS calls `POST https://push.yakteam.com/v1/notify` (type=voip / missed / message) to push inbound calls and SMS to YakPhone.

---

## 2. Hardware Preparation

| Part | Description | Notes |
|---|---|---|
| QDC507 4G module | DJI Gen-1 4G module (Baiwang firmware), USB ID `2C7C:0125`, containing a Qualcomm MDM9607 + eSIM/physical SIM slot | Must have **adb support** (an adb port in USB enumeration), otherwise the module runtime can't be pushed |
| SIM card | Any of the three major domestic carriers (tested with China Unicom here); VoLTE must be provisioned | Call quality depends on VoLTE registration (`AT+CEREG`=1) |
| NAS / server | Any x86_64 / arm64 Linux with a USB port that can run Tailscale | Tested on: fnOS (Feiniu) arm64 |
| iPhone | iOS 12+ (tested up to iOS 26) | Install YakPhone (App Store: Yak – AI SIP Phone) |
| USB cable | Data cable (not charge-only) | — |

**Module USB enumeration check** (after plugging it into the NAS):

```bash
lsusb            # should show BAIWANG / 2C7C:0125
ls -la /dev/ttyUSB*          # ttyUSB0(diag) ttyUSB1(nmea) ttyUSB2(AT) ...
cat /proc/asound/cards       # should show "BAIWANG Baiwang ... (USB Audio)" -> hw:0,0
```

**Step 1: identify the AT serial port (it may not be ttyUSB2!)**

The module enumerates 4 serial ports over USB, with fixed roles:

```text
ttyUSB0 = Diag (diagnostics)
ttyUSB1 = NMEA (GPS)
ttyUSB2 = AT (dial/SMS/config)   ← most commonly ttyUSB2
ttyUSB3 = Modem (PPP data)
```

> ⚠️ **Don't guess by the number**: enumeration order varies across boards/USB hubs (the AT port is sometimes ttyUSB4 or ttyUSB6). **Identify it with commands — don't trust the number**:

```bash
# Method 1 (recommended): the repo's built-in script auto-detects it
git clone https://github.com/mccding/CellBridge   # if you haven't cloned it yet
./CellBridge/scripts/detect-at-port.sh            # prints the AT port path, e.g. /dev/ttyUSB2

# Method 2 (manual): send AT to each port and see which one answers OK
for p in /dev/ttyUSB*; do
  echo "--- $p ---"
  (stty -F $p 9600 raw -echo; printf 'AT\r' > $p; timeout 1 cat $p) 2>/dev/null
done
# the port whose output shows "OK" is the AT port

# Method 3 (stable by-id path; stays the same across reboots once configured):
ls -l /dev/serial/by-id/ | grep -E "if0[2-9]|if03"   # find ...-if02-... or ...-if03-...
# the AT port is usually the by-id ending in if02 or if03 (BAIWANG_Baiwang-if02-port0, etc.)
```

Once you know the AT port, every AT command below uses `<AT-port>` to mean it (e.g. `/dev/ttyUSB2` or a by-id path).

**Step 2: unlock adb root (mandatory for new-user modules — factory-locked!)**

At the factory the module's adb is locked behind a **QADBKEY challenge** — `AT+QADBKEY?` returns a random 8-digit challenge (e.g. `+QADBKEY: 12345678`). Until it is unlocked, `adb devices` shows nothing and writing the adb bit in usbcfg is rejected.

**The unlock is public**: Quectel's QADBKEY challenge-response uses the **fixed secret `SH_adb_quectel`** (see [dji-voice-go/qadbkey.go](https://github.com/iniwex5/dji-voice-go) and other open-source implementations). Compute glibc MD5-crypt (`$1$<challenge>$`) over the fixed secret and take the 22-character hash segment as the unlock password; `AT+QADBKEY="<password>"` succeeds and stays unlocked **persistently across reboots (once only)**.

**One-shot (auto-detect AT port → auto-unlock adb root → write usbcfg 7×1 → reboot)**:

```bash
./scripts/init-module.sh             # one command: factory-locked modules are unlocked & enabled
./scripts/init-module.sh /dev/ttyUSB2   # specify the AT port explicitly
./scripts/init-module.sh --check     # status only, writes nothing
```

Manual equivalent steps (to understand the mechanics):

```bash
# 1. Get the challenge
AT+QADBKEY?                          # → +QADBKEY: 12345678
# 2. Unlock password = 22-char hash segment of MD5-crypt(SH_adb_quectel, $1$12345678$)
#    e.g. `openssl passwd -1 -salt 12345678 SH_adb_quectel` → $1$12345678$UnSn...,
#    the password is the "UnSn..." part (22 chars)
# 3. Unlock (persistent)
AT+QADBKEY="<YOUR-22-char-password>"  # → OK
# 4. Verify: adb devices shows the device
```

After unlocking, adb runs as **root** (`adb shell id -u` → `0`), which the gateway needs to push its runtime and `insmod` kernel modules.

> An already-unlocked module rejects a repeat unlock (non-OK) — that is normal and the script handles it. **If your module had adb enabled before, just run the script; it detects the state and skips.**

**Step 3: enable adb + UAC (usbcfg) and reboot to apply**

```bash
# usbcfg parameter bits: Dload,AT,Modem,NMEA,Diag,ADB,UAC — all 1 = everything on
printf 'AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1\r' > <AT-port>
printf 'AT+CFUN=1,1\r' > <AT-port>     # reboot the RF subsystem (USB re-enumerates, ~15s)
```

Verify after the reboot (an adb port + an audio device should now appear):

```bash
lsusb                     # 2C7C:0125 still visible
adb devices -l            # QDC507 device appears (transport usb:...)
cat /proc/asound/cards    # BAIWANG ... (USB Audio) -> hw:0,0 appears
```

> ⚠️ **Don't change any Tailscale / fnOS system config**. The module's working mode is fixed to UAC+adb.

**Module-side voice runtime components** (placed on the NAS; `runtime_dir` points here; the files come from the module vendor / are user-supplied and **are not distributed with the open-source repo**):

```text
/opt/cellbridge/module-voice/
├── qdc507_aprv3.ko          # APR v3 kernel module (fixed-hash checked)
├── qdc507_voice.ko          # Voice/AFe kernel module (fixed-hash checked)
├── mavo-pcm-bridge.armv7    # MaVo PCM routing binary (--voice-route-session)
└── alsaucm                  # audio-route calibration tool (contains the VoLTE verb config)
```

---

## 3. Tailscale Networking

### 3.1 Install and log in on the NAS (Tailscale authkey)

```bash
# 1. Install Tailscale on the NAS (fnOS/Debian-family):
#    curl -fsSL https://tailscale.com/install.sh | sh
#    or follow https://tailscale.com/download for your distro

# 2. Get an authkey (login credential):
#    Tailscale Admin Console → Settings → Keys → Generate auth key
#    → copy the generated `tskey-auth-...` one-time key

# 3. Log into your tailnet with the authkey (run once only):
tailscale up --authkey=tskey-auth-XXXX --hostname cellbridge-nas

# 4. Verify it's online and note the tailnet IP:
tailscale status
tailscale ip -4        # note it, e.g. 100.x.y.z (the config below uses the hostname instead)
```

### 3.2 Install and log in on the iPhone

1. Install **Tailscale** from the App Store and log in with the **same tailnet account** (the same account as the NAS; the authkey is only used once on the NAS — on iPhone just log in with your account credentials).
2. Verify: the Tailscale app on iPhone shows both devices online.

### 3.3 Enable MagicDNS (required — the SIP config depends on a hostname)

In the Tailscale Admin Console → **DNS**, check **MagicDNS** to enable it.

> Hostname format: `<hostname>.<tailnet-name>.ts.net` (e.g. `cellbridge-nas.<your-tailnet>.ts.net`). Every `<your-nas>.ts.net` below refers to it.

### 3.4 Network checks

1. Ping the NAS hostname from the iPhone:
   ```bash
   # on the iPhone (or any machine inside the tailnet)
   ping cellbridge-nas.<your-tailnet>.ts.net
   ```
2. Direct connection inside the tailnet works by default — **no ACL changes are needed**.

> Why no TURN: the two devices connect directly inside the tailnet — no NAT, no relay, so `turn:` stays empty. The coturn:3478 from the old WebRTC days is a leftover; this path doesn't use it.

---

## 4. Building the Gateway for arm64 and x86

### 4.1 Source layout

```text
cellbridge/
├── gateway/
│   ├── cmd/cellbridge-gateway/     # main program (SIP + RTP + SMS + voice-route management)
│   ├── internal/
│   │   ├── sip/                    # SIP server (register/outbound/inbound/MESSAGE SMS)
│   │   │   ├── server.go           # server main logic (INVITE/BYE/MESSAGE…)
│   │   │   ├── session.go          # call session (Dial/AwaitBridge/Hangup, 15s bounded timeout)
│   │   │   ├── media.go            # MediaSession RTP (seq++, ts+=160, PCMU/8000)
│   │   │   ├── registrar.go        # registrar
│   │   │   ├── auth.go             # Digest auth
│   │   │   └── yakpush.go          # push.yakteam.com push (voip/missed/message)
│   │   ├── voice/
│   │   │   ├── alsa.go             # NAS-side arecord/aplay management (capture-first ordering)
│   │   │   ├── bridge.go           # PCM↔RTP bridge + stats logging (peak/nonzero/mean)
│   │   │   └── qdc507/audio.go     # module-side runtime: insmod/mavo/calibration/per-call rotating
│   │   ├── modem/at/               # AT adapter (SMS short-code text / long-number PDU, dry-run guard)
│   │   ├── api/server.go           # HTTP API (8787) + SMS engine wiring + push
│   │   └── sms/engine.go           # SMS engine (store + send + push)
│   └── Dockerfile                  # multi-stage build (TARGETARCH switchable)
└── infra/
    ├── config.example.yaml          # generic config template
    └── config.nas-qdc507.example.yaml  # QDC507 NAS-specific template
```

### 4.2 Local cross-compilation (recommended, fastest)

```bash
cd gateway

# arm64 (common on fnOS/Raspberry Pi/NAS)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /tmp/cellbridge-gateway-arm64 ./cmd/cellbridge-gateway

# x86_64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /tmp/cellbridge-gateway-amd64 ./cmd/cellbridge-gateway
```

**One-shot script (recommended — produces both architectures at once)** — the repo ships `scripts/build.sh`:

```bash
./scripts/build.sh          # produces both arm64+amd64 binaries + SHA256SUMS into artifacts/ in one go
./scripts/build.sh docker   # additionally builds the Docker image (host architecture)
./scripts/build.sh docker-all  # buildx dual-platform image (arm64+amd64)
```

### 4.3 Dual-platform Docker images (x86 + arm)

The repo ships `gateway/Dockerfile`, which supports `TARGETARCH`:

```bash
cd cellbridge

# Single architecture
docker build --build-arg TARGETARCH=arm64 -f gateway/Dockerfile -t cellbridge-gateway:arm64 .
docker build --build-arg TARGETARCH=amd64 -f gateway/Dockerfile -t cellbridge-gateway:amd64 .

# Both platforms in one push (requires buildx + a remote builder, or a manifest on the same arch)
docker buildx build --platform linux/arm64,linux/amd64 \
    --build-arg TARGETARCH=placeholder \
    -f gateway/Dockerfile -t yourrepo/cellbridge-gateway:latest --push .
```

> The Dockerfile uses `golang:1.25-bookworm` as the build base and `debian:bookworm-slim` as the runtime image (ships alsa-utils, so no extra arecord/aplay install is needed on the NAS). Note: `--build-arg TARGETARCH=placeholder` in the Dockerfile relies on buildx auto-injecting `TARGETARCH` (a Docker built-in variable); for a single architecture, pass `--build-arg TARGETARCH=arm64` explicitly.

### 4.4 Automatic builds with GitHub Actions (works out of the box after open-sourcing)

The repo ships `.github/workflows/build.yml` — **pushing a tag automatically produces every artifact**:

```bash
git tag v1.0.0 && git push origin v1.0.0
```

The workflow does three things automatically:

| Step | Output |
|---|---|
| `binaries` job (matrix: arm64+amd64) | `cellbridge-gateway-linux-arm64` / `-amd64` static binaries |
| `docker` job (buildx dual-platform) | `ghcr.io/<your-repo>:v1.0.0` + `:latest` (linux/arm64 + linux/amd64) |
| `release` job (on tag) | GitHub Release + both binaries + `SHA256SUMS.txt` |

Manual trigger: on the GitHub repo page, **Actions → build → Run workflow** — you can build without pushing a tag.

### 4.5 Unit tests

```bash
cd gateway
go vet ./...
go test ./...        # covers: RTP timing, SIP retransmit dedup, SMS dry-run (CMGW), modem URC parsing
```

---

## 5. Deploying to the NAS

### 5.0 One-command deployment (recommended — the only command you need)

The repo ships `scripts/deploy.sh` — **build → backup → upload → replace → restart → verify** in one command:

```bash
./scripts/deploy.sh                      # deploys to <your-nas> by default (tailnet SSH hostname)
NAS_HOST=my-nas ./scripts/deploy.sh      # specify the NAS hostname
./scripts/deploy.sh --no-build           # skip the build, only upload existing artifacts
```

Prerequisites: this machine is on the tailnet and `tailscale ssh root@<NAS>` works. Before deploying, the script automatically backs up the currently running binary on the NAS as `.pre-deploy-<timestamp>`, so you can roll back at any time if something fails.

### 5.1 Manual deployment (to understand the details)

Directories and the systemd service (bare install, not Docker — paths tested on real hardware):

```bash
# on the NAS
mkdir -p /mnt/docker-compose/cellbridge-gateway/{bin,data}
scp /tmp/cellbridge-gateway-arm64 root@<NAS>:/tmp/cellbridge-gateway.new
ssh root@<NAS> 'mv /tmp/cellbridge-gateway.new /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway && chmod +x .../bin/cellbridge-gateway'
```

systemd service (`/etc/systemd/system/cellbridge-gateway.service`):

```ini
[Unit]
Description=CellBridge Gateway (Tailnet-only)
Wants=network-online.target tailscaled.service
After=network-online.target tailscaled.service docker.service

[Service]
Type=simple
User=root
EnvironmentFile=/mnt/docker-compose/cellbridge-gateway/turn.env
# --listen 127.0.0.1 = HTTP API binds to the host only (secure); external access goes through the
# tailnet hostname (Tailscale Serve 443 → 127.0.0.1:8787)
ExecStart=/mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway \
    --config /mnt/docker-compose/cellbridge-gateway/config.yaml \
    --listen 127.0.0.1:8787 \
    --data-dir /mnt/docker-compose/cellbridge-gateway/data
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now cellbridge-gateway
systemctl is-active cellbridge-gateway    # -> active
ss -lnup | grep 5060                      # confirm the SIP UDP listener
```

### 5.2 Deployment rollout flow (safe, avoids overwriting in place)

```bash
scp cellbridge-gateway-arm64 root@<NAS>:/tmp/cellbridge-gateway.new
ssh root@<NAS> '
  mv /tmp/cellbridge-gateway.new /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway
  chmod +x /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway
  systemctl restart cellbridge-gateway
  sleep 4
  systemctl is-active cellbridge-gateway'
```

> ⚠️ `systemctl stop` may be blocked by the NAS security policy; `restart` usually works. When it's blocked, hand the manual commands to the user to run.

---

## 6. config.yaml and SIP Parameters

### 6.0 User-required values (three credentials, all mandatory)

Before creating `config.yaml`, gather these three values — **they all come from you; the repo doesn't provide them**:

| # | Credential | Where to get it | Where it goes in config |
|---|---|---|---|
| 1 | **NAS ts.net hostname** | After enabling MagicDNS in §3.3, the `<hostname>.<tailnet-name>.ts.net` shown by `tailscale status` on the NAS | `network.tailnet_hostname` |
| 2 | **SIP account + password** | **You define them** (example `1001`; the password is your choice — it must be entered verbatim into YakPhone later) | `sip.users[0].username/password` |
| 3 | **YakPhone push token** | **Inside the YakPhone app**: open Yak → Settings/Account page → copy the PushKit token (a Base64 string) | `sip.push_token` |

> ⚠️ All three are private credentials: don't commit them to git or share screenshots. Without the YakPhone token, **incoming calls and SMS won't push** (no ringing when the app is closed).

**The final config as verified** (deployed at `/mnt/docker-compose/cellbridge-gateway/config.yaml` on the NAS; put your own YakPhone push token where `push_token` is):

```yaml
network:
  mode: tailnet
  tailnet_hostname: <your-nas>.ts.net   # ← replace with your NAS ts.net hostname
  public_fallback: false
server:
  listen: 127.0.0.1:8787   # HTTP API binds to loopback only (secure default); external access goes via the tailnet hostname
  public_base_url: ""
data:
  dir: /mnt/docker-compose/cellbridge-gateway/data
modem:
  adapter: qdc507
  tty: /dev/serial/by-id/usb-BAIWANG_Baiwang-if02-port0   # stable by-id path (don't use the bare ttyUSB2 name)
  baud: 9600
  allow_usb_identity_changes: false
voice:
  enabled: true
  backend: qdc507
  codec: pcmu
  sample_rate: 8000
  rx_path: hw:0,0
  tx_path: hw:0,0
  runtime_dir: /opt/cellbridge/module-voice
sip:
  enabled: true
  listen: 0.0.0.0:5060   # SIP listens on all interfaces (tailnet clients connect via the ts.net hostname); 0.0.0.0 = bind address, not a connection target
  realm: cellbridge
  push_token: "<your YakPhone push token>"      # [REDACTED in repo example]
  users:
    - username: "1001"
      password: "<your SIP password>"           # [REDACTED in repo example]
webrtc:
  remote_ice_policy: relay
  local_ice_policy: all
  media_network_policy: tailnet-turn
  turn_credential_ttl: 10m
  private_turn:
    enabled: false           # ← TURN disabled in phase 1 (direct tailnet)
    host: ""
    port: 3478
    bind_ip: ""
push:
  mode: broker
  broker_url: https://push.example.com    # legacy APNs channel placeholder; actual SMS/call push goes via push.yakteam.com
recording:
  enabled: true
  retention_days: 90
  minimum_free_bytes: 524288000
  auto_mode: off
security:
  pairing_local_only: true
  admin_local_only: true
  redact_logs: true
```

**SIP parameter baseline (don't change):**

| Parameter | Value | Notes |
|---|---|---|
| Hostname | `<your-nas>.ts.net` | tailnet MagicDNS |
| Port | 5060/UDP | no TLS in phase 1 |
| Account | `1001` / realm `cellbridge` | Digest auth |
| Codec | PCMU (PCMA=0) / 8000 Hz / mono | baresip parses strictly; 2xx must carry Contact+Allow |
| SDP | `c=IN IP4 <tailnetIP>` | RTP destination = INVITE source IP + SDP port (**ignore the SDP c= line IP** — YakPhone writes a public IP) |
| TURN/STUN/SRTP | all empty/disabled | direct tailnet, no NAT |

---

## 7. YakPhone Client Configuration

1. Install **Yak – AI SIP Phone** from the App Store (baresip kernel):
   👉 [https://apps.apple.com/in/app/yak-ai-sip-phone/id6763033863](https://apps.apple.com/in/app/yak-ai-sip-phone/id6763033863) (free)
2. **Get the PushKit token (do this before configuring the NAS)**:
   YakPhone → Settings → Account/Push page → **copy Push Token** (a Base64 string, shaped like `AAA...==`) → paste it into `sip.push_token` in the NAS `config.yaml`.
   > If you can't find that page in the app: look for "PushKit" or "APNs Token" in Yak's settings; the entry name may vary slightly between versions.
3. Account settings:
   - **Server**: `<your-nas>.ts.net:5060`
   - **Username**: `1001`; **Password**: matching the config
   - **Transport**: UDP
   - **Codec order**: PCMU (or G711u) first; **don't use Opus in phase 1**
4. Once registered, the dialer shows a "Registered" status.
5. SMS: in YakPhone's conversation/messages screen, enter the **full phone number** (e.g. `13800138000`) and the content, then send — YakPhone sends a **SIP MESSAGE** to the NAS, which relays it via AT+CMGS to actually send it.

> If the dial/SMS buttons don't respond: make sure `journalctl -u cellbridge-gateway -f` on the NAS shows `sip register user=1001`; if it doesn't, registration never reached the NAS (check the network segment/firewall).

---

## 8. Voice Path

### 8.1 Outbound — final state machine

```text
YakPhone → INVITE (sip:10010@domain)
NAS: 100 Trying
NAS: Dial() → ① PrepareRoute: force a rotate before every call (killall mavo → rebuild session,
      bypassing the UAC firmware bug where capture dies after aplay touches it)
      ② ATD10010; ③ WaitActive: poll AT+CLCC until state=0 (active)
NAS: 180 Ringing (Contact: <sip:cellbridge@IP>)
NAS: Cellular answered → AwaitBridge(queue):
      ensureRuntime (shortcut route already prepared) → ALSAAudio.Start (capture first, aplay second)
NAS: 200 OK (Contact + Allow + SDP c=tailnetIP)
YakPhone: ACK (NAS does not reply; discarded)
Bidirectional RTP: PCMU/8000, NAS rtp = INVITE source IP + SDP port, seq++/ts+=160
BYE → 200 OK → Hangup(): teardown bridge (kill arecord/aplay) + modem line (15s bounded ATH)
```

**Key implementation details (don't regress):**
- **RTP sequence/timestamps must increment**: `seq++`, `ts+=160` (per 20ms PCMU frame). A constant 0 makes baresip discard the packets as duplicates per RFC3550 → silence.
- **Never reply 200 to ACK**: ACK closes the INVITE transaction; replying 200 wedges the YakPhone state machine in ringing.
- **200 OK must carry `Contact` + `Allow`**: `<sip:cellbridge@IP>` + `Allow: INVITE, ACK, BYE, CANCEL, OPTIONS`; missing Contact → baresip reports badmessage.
- **modem ended events clear active**: NO CARRIER / an unanswered call → clear the `active` flag, otherwise the next call always 500s with "active modem call exists".
- **ALSA start timing**: **arecord/aplay must only start after the cellular leg is active (CLCC active)**. Starting at dial time = UAC not yet streaming → permanently all-zero. Start capture first, aplay second.
- **Serial deadlock protection**: every Dial/Hangup AT exchange runs under a 15s bounded timeout; on timeout the ioMu lock is released automatically (otherwise one stuck ATH wedges every later dial).
- **Call-ID session key + retransmit dedup**: retransmitted INVITEs match the existing session by Call-ID and get 180/200 without re-dialing.

### 8.2 Inbound calls

```text
Module: RING / +CLIP (AT+CLIP=1, CCWA=1 must be preset)
NAS HandleURC → incoming event (peer = calling number)
NAS ringClients: send INVITE (with SDP) to all registered SIP accounts
  + in parallel POST push.yakteam.com (type=voip, caller_uri, caller_name)
YakPhone rings (CallKit) → answer → ACK (discarded by NAS) + bidirectional RTP
NO CARRIER / BYE → clear active + release media
```

### 8.3 Voice-route management (QDC507)

Before every call (in `Dial()`'s `PrepareRoute`):

```text
1. killall -9 mavo-pcm-bridge.armv7 (⚠️ must be -9 + verify zeroed: [m]avo regex prevents pgrep self-match)
2. If no mavo leftovers: adb push the runtime (qdc507_aprv3.ko/qdc507_voice.ko/mavo-pcm-bridge)
   to /run/maccellular-call/ on the module (sbc hash check)
3. insmod aprv3 → voice (idempotent: test -d /sys/module/xxx || insmod)
4. alsaucm calibration (VoLTE verb + AuxPcm Rx/Tx enable, logs "VoLTE enable 1")
5. sh /data/celldock-route-launch.sh to start mavo (nohup + pid file /run/celldock-voice-route.pid)
6. Poll routeReadyScript (pid file/process/log "VoLTE route session active on hw:0,4"/audio_enable/hw:0,4 RUNNING) until "ready"
7. echo 1 > /sys/class/android_usb/f_audio/audio_enable
```

> ⚠️ **Rotating every call is the key**: the QDC507 UAC firmware's capture stream goes silent once arecord/aplay touches it, and only **rebuilding the mavo session** restores it. Verified: with routine rotating → many consecutive calls stay at full-strength nonzero=250.

---

## 9. SMS Path

### 9.1 Sending (YakPhone → SIP MESSAGE → AT+CMGS)

```text
YakPhone: MESSAGE sip:13800138000@domain SIP/2.0
          Content-Type: text/plain
          <body>
NAS: handleMessageRequest:
     parse Request-URI number + body → SMSEngine.Send(ctx, to, body)
     → adapter.SendSMS:
        short code ≤6 digits (e.g. 10010) → AT+CMGF=1 + AT+CMGW/CMGS="short-code" text mode
        long number >6 digits         → AT+CMGF=0 + PDU (AT+CMGS=<tpdu length> + PDU + 0x1A)
NAS → 200 OK
```

**Example SMS PDU** (long number, e.g. 185...): `0001000B818155118865F50008044F60597D` (the "你好" / "hello" message). Short codes use text mode to avoid the PDU `>` prompt being stolen by the tty.

**HTTP API send** (equivalent to MESSAGE, for scripts/integrations; use 127.0.0.1 when running on the NAS itself, otherwise go through the tailnet hostname):

```bash
curl -X POST http://127.0.0.1:8787/api/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ***" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"to":"13800138000","body":"测试"}'
```

### 9.2 Receiving (SIM → +CMTI → store → push)

```text
Module +CMTI: "ME",<idx> → NAS AT+CMGL fetch → decode (gsm7/ucs2) → store in the messages table
→ sendMessagePush():
   YakPush(type=message, caller_uri=sip:<sender>, message_body=<body>)  ← push.yakteam.com
   (ignored when the legacy APNs broker is not configured; no impact)
→ YakPhone receives the notification; opening it shows the SMS
```

### 9.3 Call-charge protection (important!)

- The code ships a **dry-run switch**: the `CELLBRIDGE_SMS_DRY_RUN` environment variable, **default true**.
- dry-run=true: SMS goes through `AT+CMGW` (stored in the module's ME only, **not sent, not billed**).
- dry-run=false: uses `AT+CMGS` (actually sent, billed by the carrier).
- To enable real sending under systemd, add `Environment=CELLBRIDGE_SMS_DRY_RUN=false` to the `[Service]` section.
- For self-testing/replication, **keep dry-run on first**; switch it on only after the path is confirmed.

---

## 10. PushKit Push

YakPhone's official push endpoint (you provide the token; the repo documents the payload format):

```http
POST https://push.yakteam.com/v1/notify
Content-Type: application/json

{
  "token": "<your PushKit token>",
  "caller_uri": "sip:1000@pbx.example.com",
  "caller_name": "1000",
  "type": "voip" | "missed" | "message",
  "message_body": "SMS body (when type=message)"
}
```

- **Incoming call**: send `type=voip` on the RING/+CLIP event → YakPhone PushKit wakes up; it rings even when the app is closed.
- **Missed call**: send `type=missed` after NO CARRIER (optional).
- **SMS**: after storage, send `type=message` + `message_body` → the SMS appears in YakPhone's notification bar.
- The token is read from config `sip.push_token`; verify with: `journalctl | grep yakpush` should show `yakpush sent type=message status="200 OK"`.

---

## 11. Self-Test Scripts

The repo's `tools/` provides these (run on a Mac or the NAS; pure Python, no dependencies):

| Script | Purpose | Key output |
|---|---|---|
| `sip_fresh.py` | Full-path self-test: REGISTER(digest) → INVITE(new Call-ID) → 200 → BYE | `200 OK` |
| `sip_bye2.py` | Place a call then BYE immediately (10s window) | BYE→200 |
| `sip_dump200.py` | Print the raw 200 OK (verify Contact/Allow/SDP) | response headers |
| `readycheck.sh` | Item-by-item READY check of the module voice route (pid/mavo/log/audio_enable/hw RUNNING) | each item OK |
| `mavotest.sh` | Run the mavo startup script by hand, verify the pid file is written | `1156 7011235` format |

**Voice heartbeat verification** (check the NAS logs after each call):

```text
voice cellular stats call_id=xxx frames=250 peak_max=32768 nonzero=250 mean=9000+
```

- `nonzero=250` means 250/250 samples nonzero = **real voice**;
- `nonzero=0`, or a constant `peak_max=32768` with no nonzero = **silence/saturation** — troubleshoot per §12.

---

## 12. Eight-Layer Troubleshooting Methodology (§36)

When you hit "silence / one-way audio / dial failure", work bottom-up layer by layer; **a layer only counts as passed with clear evidence**:

```text
Layer 1  QDC507 firmware: UAC=1 (last usbcfg bit), AT+CFUN=1,1 applied
Layer 2  ALSA: /proc/asound/card0/pcm0c/sub0/status = RUNNING
         (during a call, watch arecord io: wchar should keep growing; frozen = X RUN / dead pipe)
Layer 3  PCM peak: manual arecord on the NAS during a call should peak at 32768, more than half nonzero
Layer 4  SIP: INVITE → 100/180 → 200 OK → ACK (retransmits deduped by Call-ID)
Layer 5  RTP: bidirectional packet counts (tcpdump udp port 40000-40100 or gateway stats)
Layer 6  Codec: PCMU/8000/mono (not Opus)
Layer 7  YakPhone: registration status, dial/answer buttons, speaker
Layer 8  iOS: whether CallKit has taken over, Audio Session route
```

**Classic quantitative checks**:

```bash
# 1. ALSA status
cat /proc/asound/card0/pcm0c/sub0/status
# 2. Manual capture to grab real voice (while the gateway is stopped)
timeout 10 arecord -q -D hw:0,0 -t raw -f S16_LE -r 8000 -c 1 -d 8 /tmp/x.raw
python3 -c "import struct;d=open('/tmp/x.raw','rb').read();s=struct.unpack('<%dh'%(len(d)//2),d[:len(d)//2*2]);print('peak',max(abs(int(x)) for x in s),'nonzero',sum(1 for x in s if x!=0))"
# 3. Gateway stats (one line every 250 frames, i.e. every 5s)
journalctl -u cellbridge-gateway -f | grep 'voice cellular stats'
```

---

## 13. Known Pitfalls and Workarounds

| # | Symptom | Root cause | Fix (built into the code) |
|---|---|---|---|
| 1 | Dial returns 500 | 200 OK missing `Contact`; baresip's strict parsing reports badmessage | 2xx must carry Contact+Allow |
| 2 | Redial after hangup returns 500 | NO CARRIER from an unanswered call doesn't clear the modem active flag | ended-type URCs clear active automatically |
| 3 | Connected but completely silent | RTP sequence/timestamps stuck at 0 → baresip drops everything as duplicates | media.go seq++ / ts+=160 |
| 4 | Call rings forever after 180 | ACK answered with 200 → YakPhone state machine wedged in ringing | discard ACK outright |
| 5 | Dial hangs (and everything after it) | the 22:39 BYE's ATH exchange has no timeout and holds the ioMu serial lock forever | Dial/Hangup 15s bounded timeout |
| 6 | arecord started at dial time → permanently all-zero | UAC gadget not yet streaming; confirmed by the "open capture before the call" experiment | WaitActive (CLCC) — only start the pair after the leg is active |
| 7 | First call has audio, all later ones are silent | **UAC capture mutes once aplay touches it** (firmware bug); leftover mavo processes clobber audio_enable | **rotate every call**: killall -9 + [m]avo pgrep to avoid self-match + rebuild a single mavo session |
| 8 | `survivors=1` infinite retry → rotate stuck → dial failure | `pgrep -f mavo-pcm-bridge` matches the executing shell itself | `pgrep -f [m]avo-pcm-bridge.armv7` |
| 9 | All calls silent after a module cold reset | ko not reloaded after reboot; prepared cache fools the check | routeAlive real probe + full bootstrap self-healing |
| 10 | pid file can't be written → READY never passes | inline adb shell script quoting broken across layers (`case \"$pid` quadruple escaping) | write the file via base64, then run with `sh` |
| 11 | `hw:0,4 busy errno 16` | a stray mavo holds the VoLTE PCM | stopRemoteRoute killalls directly, doesn't rely on the pid file |
| 12 | readLoop dies after one frame | bridge bound to AwaitBridge's answerCtx (defer cancel kills it) | bridge uses a session-level ctx |
| 13 | Occasional panic when restarting the gateway | race between Close() and URC: send on closed channel | HandleURC recover fallback |
| 14 | mavo process can't be killed | old process in D state / adb environment | killall -9 + verify loop |
| 15 | SMS accidentally sent, billing charges | code defaulted to actually sending via AT+CMGS | `CELLBRIDGE_SMS_DRY_RUN` defaults to true (CMGW) |
| 16 | Short-code SMS PDU stuck at the `>` prompt | the input prompt is stolen by the tty | short codes (≤6 digits) use AT+CMGF=1 text mode |
| 17 | SDP c= holds a public IP → RTP goes out to the public internet and never comes back | YakPhone's SDP uses the home-broadband egress address | RTP destination = INVITE source IP + SDP port |
| 18 | No 200 after 180 while ringing | stacked causes: route-teardown timing / pid escaping / killall leftovers / routePrepared shortcut | combined fixes of 7/9/10 above |
| 19 | "Stuttering" during a call (3-7s of silence then bursts) | second-level jitter of iPhone RTP over Tailscale → transient aplay starvation → **UAC ADAPTIVE playback clock stalls → ASYNC downlink capture is throttled** (a 50-frame window takes 3-7s) | **uplink jitter buffer**: RTP frames go into a 50-frame ring buffer, written to aplay smoothly on a fixed 20ms metronome; write silence when the queue is empty to avoid XRUN, drop the oldest frame when full to cap latency (bridge.go) |
| 20 | Occasional frame drops on the cellular side (frame rate falls to ~19fps) | UAC ASYNC capture jitter exceeds ALSA's default buffer | add `--buffer-size=8192 --period-size=1024` to arecord/aplay (same flags as chan-quectel; a 60s test showed zero drops) |
| 21 | **Calls and SMS all break after reboot** (systems with a system adb daemon, e.g. fnOS) | fnOS auto-starts a system adb daemon at boot (tcp:5037), and **adb's USB transport discovery is a single-listener model** — once the system daemon grabs it, the gateway's own adb can never enumerate the module (symptoms: dial freezing, probe reports "no ready ADB device") | the gateway `pkill`s the system adb daemon before every adb call and runs its own private daemon (`-L tcp:localhost:5038`). **Plain Linux (Debian/Synology, etc.) has no system adb daemon and never hits this; the fix is an invisible safety net there** |
| 22 | Companion to 21: cold-start probe cut off by the 8s timeout → voice degrades to control-only | USB re-enumeration + adb server cold start exceeds 8 seconds | probe timeout raised to 30s; transportID retries 8×750ms |

---

## 14. Security and Call-Charge Protection

- **Dry-run by default**: without `CELLBRIDGE_SMS_DRY_RUN=false`, SMS are only ever stored, never sent — no accidental billable texts.
- **Tokens/passwords**: all repo examples are `[REDACTED]`; `push_token`, SIP passwords and device tokens are filled in by the user and never committed to the repo.
- **No blanket lockdown**: SIP registration still accepts any configured account; the API requires a bearer token (generated during device pairing).
- Enable `redact_logs: true` and sensitive fields are masked automatically in logs.

---

## 15. Open-Source Information and Legal

- **Only the replicable parts are open source**: the gateway source, the infra example configs, the tools self-test scripts, and this document.
- **Not distributed with the repo**: the QDC507 module-side runtime (qdc507_aprv3.ko / qdc507_voice.ko / mavo-pcm-bridge / alsaucm) — these are the module vendor's closed-source binaries; you must obtain them yourself (see the DJOneHub project and the firmware materials bundled with the module).
- Reference projects: https://github.com/ZenGeekLabs/DJOneHub (a DJI Gen-1 4G module macOS management tool — the USB-channel/eSIM implementation is worth referencing); https://github.com/AmorXxx/iPhone_air_esim_tutorial (an EC20-family UAC + asterisk-chan-quectel SMS/voice bridging approach).
- License: see the repo LICENSE (suggested: MIT for the source; CC-BY-4.0 for the docs).
