<div align="center">

# CellBridge

**Turn any QDC507 4G module + SIM card into a personal cellular gateway for your iPhone.**

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://img.shields.io/badge/CI-GitHub%20Actions-blue)](.github/workflows/build.yml)

**[简体中文](README.zh-CN.md) · [中文入口](README.md)**

</div>

---

**CellBridge** is a self-hosted cellular gateway that bridges a physical SIM card (installed in a DJI first-generation 4G module, the QDC507) to any **SIP softphone over your private Tailscale network**. With **YakPhone** (free on the App Store) and your own **push token**, you get a fully functional second phone number on your iPhone:

- 📞 **Outbound calls** — dial any number over VoLTE
- 📲 **Inbound calls** — ringing + PushKit wake-up on your iPhone
- 💬 **SMS** — send & receive text messages over the cellular module
- 🧱 **No proprietary cloud** — it all runs on your own NAS

## Architecture

```
iPhone (YakPhone)  <──SIP/RTP (PCMU 8000)──>  NAS Gateway  <──USB/UART──>  QDC507 4G Module  <──VoLTE──>  Cellular
     └── PushKit (push.yakteam.com) ◄── incoming calls / SMS ◄──┘
```

| Component | Role |
|---|---|
| `gateway/` | Go SIP server (UDP 5060), RTP bridge, SMS engine, voice-route manager |
| `infra/` | Docker image, config templates, systemd unit |
| `tools/selftest/` | SIP full-chain self-tests (no dependencies) |
| `scripts/build.sh` | One-shot build for **arm64 + amd64** |
| `docs/CellBridge.md` | Full replication guide (Chinese) — **the single source of truth** |

## Quick Start

```bash
# 1. Build the gateway (arm64 + amd64)
./scripts/build.sh

# 2. Configure
cp infra/config.nas-qdc507.example.yaml config.yaml
#    → fill in: your NAS ts.net hostname, SIP user/password, YakPhone push token

# 3. Deploy to your NAS (via systemd or Docker)
#    see docs/CellBridge.md §5 for the exact steps

# 4. Configure YakPhone on your iPhone
#    server: <your-nas>.ts.net:5060 · user: 1001 · codec: PCMU
```

That's it — you only need **YakPhone + your push token**. Nothing else is private.

## Releases

Push a tag and GitHub Actions automatically produces:

| Artifact | Where |
|---|---|
| `cellbridge-gateway-linux-arm64` / `-amd64` | GitHub Release |
| Docker multi-arch image (`linux/arm64,linux/amd64`) | `ghcr.io/<repo>:<tag>` |

```bash
git tag v1.0.0 && git push origin v1.0.0
```

## Requirements

- **QDC507** 4G module (DJI 1st-gen, BAIWANG USB firmware, adb-enabled) + active SIM with VoLTE
- **NAS** (arm64 or amd64 Linux) with USB and Tailscale
- **iPhone** with **YakPhone** and a **push token** (obtained from the YakPhone provider)

> ⚠️ The module-side voice runtime (`qdc507_aprv3.ko`, `qdc507_voice.ko`, `mavo-pcm-bridge`) is vendor-closed-source. It is **not** distributed in this repo; see `docs/CellBridge.md` §2.

## Development

```bash
make test          # unit tests
make probe         # hardware probe (no destructive AT commands)
./scripts/build.sh # cross-compile both architectures
```

## License

[MIT](LICENSE) · see [NOTICE](NOTICE) for third-party notes.