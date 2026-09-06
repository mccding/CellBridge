<div align="center">

# CellBridge

**让任意 QDC507 4G 模块 + SIM 卡，成为你 iPhone 的私人蜂窝网关。**

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![CI](https://img.shields.io/badge/CI-GitHub%20Actions-blue)](.github/workflows/build.yml)

**[English](README.en.md)**

</div>

---

**CellBridge** 是一个自托管蜂窝网关：把插在 **QDC507 4G 模块**（大疆第一代 4G 模块）里的实体 SIM 卡，桥接到你 Tailscale 私网里的任意 **SIP 软电话**。配合 **YakPhone**（App Store 免费）和自己的 **push token**，你的 iPhone 就拥有了一条完整可用的第二号码链路：

- 📞 **拨出** — 通过 VoLTE 拨打任意号码
- 📲 **来电** — iPhone 振铃 + PushKit 唤醒
- 💬 **短信** — 经蜂窝模块收发短信
- 🧱 **无专有云** — 全部跑在你自己的 NAS 上

## 架构

```
iPhone (YakPhone)  <──SIP/RTP (PCMU 8000)──>  NAS 网关  <──USB/UART──>  QDC507 4G 模块  <──VoLTE──>  蜂窝网
     └── PushKit (push.yakteam.com) ◄── 来电 / 短信通知 ◄──┘
```

| 组件 | 职责 |
|---|---|
| `gateway/` | Go SIP 服务器（UDP 5060）、RTP 桥、短信引擎、语音路由管理 |
| `infra/` | Docker 镜像、配置模板、systemd 服务单元 |
| `tools/selftest/` | SIP 全链路自测脚本（零依赖） |
| `scripts/build.sh` | 一键编译 **arm64 + amd64** 双架构 |
| `docs/CellBridge.md` | **完整复刻教程（中文）— 唯一权威文档** |

## 快速开始

```bash
# 1. 编译网关（arm64 + amd64 双架构）
./scripts/build.sh

# 2. 配置
cp infra/config.nas-qdc507.example.yaml config.yaml
#    → 填入：NAS 的 ts.net 域名、SIP 账号密码、YakPhone push token

# 3. 部署到 NAS（systemd 或 Docker 均可）
#    具体步骤见 docs/CellBridge.md §5

# 4. 在 iPhone 上配置 YakPhone
#    服务器: <你的NAS>.ts.net:5060 · 账号: 1001 · 编码: PCMU
```

仅此而已 —— 你只需要 **YakPhone + 自己的 push token**，没有任何私有依赖。

## 发布流程

推送 tag，GitHub Actions 自动产出：

| 产物 | 位置 |
|---|---|
| `cellbridge-gateway-linux-arm64` / `-amd64` 二进制 | GitHub Release |
| Docker 多平台镜像（linux/arm64 + linux/amd64） | `ghcr.io/<仓库>:<tag>` |

```bash
git tag v1.0.0 && git push origin v1.0.0
```

## 硬件要求

- **QDC507** 4G 模块（大疆一代，BAIWANG USB 固件，带 adb）+ 已开通 VoLTE 的实体 SIM
- **NAS**（arm64 或 amd64 Linux），带 USB 口和 Tailscale
- **iPhone** + **YakPhone**（[App Store 下载](https://apps.apple.com/in/app/yak-ai-sip-phone/id6763033863) — Yak AI SIP Phone，免费）+ **push token**（从 YakPhone 服务商处获取）

> ⚠️ 模块侧语音运行时（`qdc507_aprv3.ko`、`qdc507_voice.ko`、`mavo-pcm-bridge`）为厂商闭源软件，**不随本仓库分发**；获取方式见 `docs/CellBridge.md` §2。

## 开发

```bash
make test          # 单元测试
make probe         # 硬件探测（不含任何破坏性 AT 命令）
./scripts/build.sh # 双架构交叉编译
```

## 许可证

[MIT](LICENSE) · 第三方说明见 [NOTICE](NOTICE)