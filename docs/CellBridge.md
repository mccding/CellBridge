# CellBridge：QDC507 4G 模块 + NAS 网关 + YakPhone 全链路（语音/短信/来电）

> 目标：让任何用户按照本文档，从零复刻一套"SIM 卡装在 QDC507 模块上，iPhone 通过 YakPhone 打 VoLTE 电话、收发短信"的完整系统。
> 本文档所有命令均为 **2026-09-06 真机实测通过** 的最终形态。系统已实现：拨出有声、来电振铃+推送、短信收发、挂断重拨无 500。

---

## 目录

1. [总体拓扑](#1-总体拓扑)
2. [硬件准备](#2-硬件准备)
3. [Tailscale 组网](#3-tailscale-组网)
4. [编译 Gateway（arm64 + x86 双架构）](#4-编译-gatewayarm64--x86-双架构)
5. [部署到 NAS](#5-部署到-nas)
6. [config.yaml 与 SIP 参数](#6-configyaml-与-sip-参数)
7. [YakPhone 客户端配置](#7-yakphone-客户端配置)
8. [语音链路（拨出/来电）](#8-语音链路拨出来电)
9. [短信链路（收发）](#9-短信链路收发)
10. [PushKit 推送（来电/短信唤醒）](#10-pushkit-推送来电短信唤醒)
11. [自测脚本](#11-自测脚本)
12. [八层排查方法论（§36）](#12-八层排查方法论36)
13. [已知坑与规避（踩坑实录）](#13-已知坑与规避踩坑实录)
14. [安全与话费保护](#14-安全与话费保护)
15. [开源信息与法律](#15-开源信息与法律)

---

## 1. 总体拓扑

```text
                ┌──────────────────────────────┐
                │           iPhone             │
                │      YakPhone (baresip)      │
                │  SIP 5060/UDP + RTP PCMU     │
                │  PushKit（来电唤醒/短信）      │
                └───────────┬──────────────────┘
                            │ Tailscale (tailnet 直连, 无 NAT)
                            ▼
                ┌──────────────────────────────┐
                │        NAS (fnOS/ARM64)      │
                │   cellbridge-gateway 进程     │
                │   ├── SIP 服务器 0.0.0.0:5060 │
                │   ├── RTP 桥 (PCMU/8000)      │
                │   ├── 短信引擎 (AT+CMGS/CMGW) │
                │   └── 语音路由管理 (mavo)      │
                └───────────┬──────────────────┘
                            │ USB (UAC: hw:0,0 + AT: ttyUSB2)
                            ▼
                ┌──────────────────────────────┐
                │   QDC507 4G 模块 (DJI 一代)   │
                │   ├── mavo-pcm-bridge (语音路由)│
                │   ├── qdc507_aprv3/voice.ko   │
                │   └── SIM 卡 (联通/移动/电信)   │
                └───────────┬──────────────────┘
                            │ 蜂窝网 (VoLTE)
                            ▼
                    10010 客服 / 任意电话号
```

**架构要点（最终定稿，勿改）：**
- **NAS 是 SIP 服务器**（不是 HTTP API）；YakPhone 直接注册 NAS:5060，不经过任何中转 HTTP。
- **语音 = SIP/RTP（PCMU 8000Hz, 第一阶段不启用 TLS/SRTP/STUN/TURN）**；tailnet 内直连无 NAT，`turn:` 全部留空。
- **VoLTE 语音路由**：模块侧 `mavo-pcm-bridge --voice-route-session` 打开 `hw:0,4`（VoLTE PCM），USB UAC（`f_audio`）把 PCM 流到 NAS 的 `hw:0,0`（arecord/aplay）。
- **短信**：YakPhone 通过 **SIP MESSAGE** 发给 NAS → NAS 用 `AT+CMGS`（短号文本 / 长号 PDU）经 ttyUSB2 发给模块。收短信 = 模块 `+CMTI` URC → NAS 入库 → `push.yakteam.com` 推送唤醒 YakPhone。
- **PushKit**：NAS 调 `POST https://push.yakteam.com/v1/notify`（type=voip / missed / message）把来电和短信推给 YakPhone。

---

## 2. 硬件准备

| 部件 | 说明 | 备注 |
|---|---|---|
| QDC507 4G 模块 | DJI 第一代 4G 模块（Baiwang 固件），USB ID `2C7C:0125`，内含高通 MDM9607 + eSIM/实体 SIM 槽 | 必须带 **adb 功能**（USB 枚举里有 adb port），否则无法推送模块运行时 |
| SIM 卡 | 国内三大运营商任意（本教程以联通实测）；需开通 VoLTE | 通话质量依赖 VoLTE 注册（`AT+CEREG`=1） |
| NAS/server | 任意 x86_64 / arm64 Linux，有 USB 口、能装 Tailscale | 实测环境：fnOS（飞牛）arm64 |
| iPhone | iOS 12+ / iOS 26 实测 | 安装 YakPhone（App Store：Yak – AI SIP Phone） |
| USB 线 | 数据线（非纯充电线） | — |

**模块 USB 枚举检查**（接上 NAS 后）：

```bash
lsusb            # 应出现 BAIWANG / 2C7C:0125
ls -la /dev/ttyUSB*          # ttyUSB0(diag) ttyUSB1(nmea) ttyUSB2(AT) ...
cat /proc/asound/cards       # 应出现 "BAIWANG Baiwang ... (USB Audio)" -> hw:0,0
```

**QDC507 固件 UAC 必须开启**（一次性的）：

```bash
# 通过 AT 口（ttyUSB2）执行；usbcfg 最后一位 = UAC 使能
printf 'AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1\r' > /dev/ttyUSB2
printf 'AT+CFUN=1,1\r' > /dev/ttyUSB2     # 重启射频子系统使配置生效（USB 会重枚举，约 15s）
```

> ⚠️ **不要改 Tailscale / fnOS 系统配置**。模块工作模式固定 UAC+adb。

**模块侧语音运行时要件**（放 NAS 上，`runtime_dir` 指向这里；文件来自模块原厂/用户提供，**不随开源仓库分发**）：

```text
/opt/cellbridge/module-voice/
├── qdc507_aprv3.ko          # APR v3 内核模块（固定 hash 校验）
├── qdc507_voice.ko          # Voice/AFe 内核模块（固定 hash 校验）
├── mavo-pcm-bridge.armv7    # MaVo PCM 路由二进制（--voice-route-session）
└── alsaucm                  # 音频路由校准工具（内含 VoLTE verb 配置）
```

---

## 3. Tailscale 组网

### 3.1 NAS 安装并登录（Tailscale authkey 步骤）

```bash
# 1. NAS 安装 Tailscale（fnOS/Debian 系）:
#    curl -fsSL https://tailscale.com/install.sh | sh
#    或按 https://tailscale.com/download 对应发行版安装

# 2. 获取 authkey（登录凭据）:
#    Tailscale Admin Console → Settings → Keys → Generate auth key
#    → 复制生成的 `tskey-auth-...` 一次性密钥

# 3. 用 authkey 登录你的 tailnet（只执行一次）:
tailscale up --authkey=tskey-auth-XXXX --hostname cellbridge-nas

# 4. 验证已上线并记录 tailnet IP:
tailscale status
tailscale ip -4        # 记录，例：100.x.y.z（下文配置用域名代替）
```

### 3.2 iPhone 安装并登录

1. App Store 安装 **Tailscale**，登录**同一个 tailnet 账号**（与 NAS 同一账号；authkey 只在 NAS 用一次，iPhone 用账号密码登录即可）。
2. 验证：iPhone Tailscale App 显示两台设备在线。

### 3.3 启用 MagicDNS（必须，SIP 配置依赖域名）

Tailscale Admin Console → **DNS** → 勾选 **MagicDNS** 开启。

> 域名格式：`<主机名>.<tailnet名>.ts.net`（例：`cellbridge-nas.<你的tailnet>.ts.net`）。下文所有 `<你的NAS>.ts.net` 都指它。

### 3.4 网络检查

1. 从 iPhone Ping 通 NAS 域名：
   ```bash
   # iPhone 上（或任意 tailnet 内机器）
   ping cellbridge-nas.<你的tailnet>.ts.net
   ```
2. 默认 tailnet 内直连即可，**不需要任何 ACL 修改**。

> 为什么不加 TURN：tailnet 内两台设备直连，无 NAT 无中继，`turn:` 留空即可。旧 WebRTC 时代的 coturn:3478 是遗留，本链路不用。

---

## 4. 编译 Gateway（arm64 + x86 双架构）

### 4.1 源码结构

```text
cellbridge/
├── gateway/
│   ├── cmd/cellbridge-gateway/     # 主程序（SIP + RTP + SMS + 语音路由管理）
│   ├── internal/
│   │   ├── sip/                    # SIP 服务器（注册/拨出/来电/MESSAGE 短信）
│   │   │   ├── server.go           # 服务器主逻辑（INVITE/BYE/MESSAGE…）
│   │   │   ├── session.go          # 通话会话（Dial/AwaitBridge/Hangup, 15s 有界超时）
│   │   │   ├── media.go            # MediaSession RTP（seq++, ts+=160, PCMU/8000）
│   │   │   ├── registrar.go        # 注册表
│   │   │   ├── auth.go             # Digest 认证
│   │   │   └── yakpush.go          # push.yakteam.com 推送（voip/missed/message）
│   │   ├── voice/
│   │   │   ├── alsa.go             # NAS 侧 arecord/aplay 管理（capture-first 时序）
│   │   │   ├── bridge.go           # PCM↔RTP 桥 + 统计日志（peak/nonzero/mean）
│   │   │   └── qdc507/audio.go     # 模块侧运行时：insmod/mavo/校准/每通 rotating
│   │   ├── modem/at/               # AT 适配器（SMS 短号文本/长号 PDU, dry-run 保护）
│   │   ├── api/server.go           # HTTP API (8787) + SMS 引擎接线 + push
│   │   └── sms/engine.go           # 短信引擎（入库 + 发送 + 推送）
│   └── Dockerfile                  # 多阶段构建（TARGETARCH 可切换）
└── infra/
    ├── config.example.yaml          # 通用配置模板
    └── config.nas-qdc507.example.yaml  # QDC507 NAS 专用模板
```

### 4.2 本地交叉编译（推荐，最快）

```bash
cd gateway

# arm64（飞牛/树莓派/NAS 常见架构）
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /tmp/cellbridge-gateway-arm64 ./cmd/cellbridge-gateway

# x86_64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o /tmp/cellbridge-gateway-amd64 ./cmd/cellbridge-gateway
```

**一键脚本（推荐，双架构同时出）**——仓库自带 `scripts/build.sh`：

```bash
./scripts/build.sh          # 一次产出 arm64+amd64 双二进制 + SHA256SUMS 到 artifacts/
./scripts/build.sh docker   # 追加构建 Docker 镜像（本机架构）
./scripts/build.sh docker-all  # buildx 双平台镜像（arm64+amd64）
```

### 4.3 Docker 双平台镜像（x86 + arm）

仓库自带 `gateway/Dockerfile`，支持 `TARGETARCH`：

```bash
cd cellbridge

# 单架构
docker build --build-arg TARGETARCH=arm64 -f gateway/Dockerfile -t cellbridge-gateway:arm64 .
docker build --build-arg TARGETARCH=amd64 -f gateway/Dockerfile -t cellbridge-gateway:amd64 .

# 双平台一份推送（需要 buildx + 远程 builder 或同一架构上做 manifest）
docker buildx build --platform linux/arm64,linux/amd64 \
    --build-arg TARGETARCH=placeholder \
    -f gateway/Dockerfile -t yourrepo/cellbridge-gateway:latest --push .
```

> Dockerfile 基础镜像 `golang:1.25-bookworm` + 运行时镜像 `debian:bookworm-slim`（自带 alsa-utils，NAS 上无需额外装 arecord/aplay）。注意：Dockerfile 里 `--build-arg TARGETARCH=placeholder` 需要 buildx 自动注入 `TARGETARCH`（Docker 内置变量），单架构时显式传 `--build-arg TARGETARCH=arm64`。

### 4.4 GitHub Actions 自动编译（开源后开箱即用）

仓库自带 `.github/workflows/build.yml`，**推 tag 自动出全部产物**：

```bash
git tag v1.0.0 && git push origin v1.0.0
```

工作流自动做三件事：

| 步骤 | 产出 |
|---|---|
| `binaries` job（matrix: arm64+amd64） | `cellbridge-gateway-linux-arm64` / `-amd64` 静态二进制 |
| `docker` job（buildx 双平台） | `ghcr.io/<你的repo>:v1.0.0` + `:latest`（linux/arm64 + linux/amd64） |
| `release` job（tag 时） | GitHub Release + 两个二进制 + `SHA256SUMS.txt` |

手动触发：GitHub 仓库页 **Actions → build → Run workflow**，不推 tag 也能编译。

### 4.5 单元测试

```bash
cd gateway
go vet ./...
go test ./...        # 覆盖：RTP 时序、SIP 重传去重、SMS dry-run(CMGW)、modem URC 解析
```

---

## 5. 部署到 NAS

### 5.0 一键部署（推荐，唯一必须的命令）

仓库自带 `scripts/deploy.sh`——**编译 → 备份 → 上传 → 替换 → 重启 → 验证** 一条命令完成：

```bash
./scripts/deploy.sh                      # 默认部署到 <你的NAS>（tailnet SSH 主机名）
NAS_HOST=my-nas ./scripts/deploy.sh      # 指定 NAS 主机名
./scripts/deploy.sh --no-build           # 跳过编译，只上传已有产物
```

前置条件：本机已加入 tailnet 且 `tailscale ssh root@<NAS>` 可用。部署前脚本自动把 NAS 上当前运行的二进制备份为 `.pre-deploy-<时间戳>`，失败可随时回滚。

### 5.1 手动部署（了解细节用）

目录与 systemd 服务（非 Docker 直装，实测路径）：

```bash
# NAS 上
mkdir -p /mnt/docker-compose/cellbridge-gateway/{bin,data}
scp /tmp/cellbridge-gateway-arm64 root@<NAS>:/tmp/cellbridge-gateway.new
ssh root@<NAS> 'mv /tmp/cellbridge-gateway.new /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway && chmod +x .../bin/cellbridge-gateway'
```

systemd 服务（`/etc/systemd/system/cellbridge-gateway.service`）：

```ini
[Unit]
Description=CellBridge Gateway (Tailnet-only)
Wants=network-online.target tailscaled.service
After=network-online.target tailscaled.service docker.service

[Service]
Type=simple
User=root
EnvironmentFile=/mnt/docker-compose/cellbridge-gateway/turn.env
# --listen 127.0.0.1 = HTTP API 仅绑定本机（安全）；外部统一走 tailnet 域名（Tailscale Serve 443 → 127.0.0.1:8787）
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
ss -lnup | grep 5060                      # SIP UDP 监听确认
```

### 5.2 部署上线流程（安全，避免直接覆盖）

```bash
scp cellbridge-gateway-arm64 root@<NAS>:/tmp/cellbridge-gateway.new
ssh root@<NAS> '
  mv /tmp/cellbridge-gateway.new /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway
  chmod +x /mnt/docker-compose/cellbridge-gateway/bin/cellbridge-gateway
  systemctl restart cellbridge-gateway
  sleep 4
  systemctl is-active cellbridge-gateway'
```

> ⚠️ `systemctl stop` 可能被 NAS 安全策略拦截；`restart` 通常可用。被拦时给出手动命令由用户执行。

---

## 6. config.yaml 与 SIP 参数

### 6.0 用户必填项（三个凭据，缺一不可）

创建 `config.yaml` 前，先备齐下列三个值——**它们全部来自你自己，仓库不提供**：

| # | 凭据 | 从哪里获取 | 填到 config 哪里 |
|---|---|---|---|
| 1 | **NAS ts.net 域名** | §3.3 MagicDNS 开启后，NAS 上 `tailscale status` 显示的 `<主机名>.<tailnet名>.ts.net` | `network.tailnet_hostname` |
| 2 | **SIP 账号+密码** | **自己定**（示例 `1001`；密码自定义，之后必须原样填进 YakPhone） | `sip.users[0].username/password` |
| 3 | **YakPhone push token** | **YakPhone App 内获取**：打开 Yak → 设置/账号页 → 复制 PushKit token（一串 Base64 字符） | `sip.push_token` |

> ⚠️ 三个值都属私有凭据：不要提交到 git、不要截图外发。YakPhone token 不填则**来电和短信不会推送**（App 未打开时无法振铃）。

**实测最终配置**（部署在 NAS `/mnt/docker-compose/cellbridge-gateway/config.yaml`；`push_token` 处放你自己的 YakPhone 推送 token）：

```yaml
network:
  mode: tailnet
  tailnet_hostname: <你的NAS>.ts.net   # ← 换成你的 NAS ts.net 域名
  public_fallback: false
server:
  listen: 127.0.0.1:8787   # HTTP API 仅绑定本机回环（安全默认）；外部一律经 tailnet 域名访问
  public_base_url: ""
data:
  dir: /mnt/docker-compose/cellbridge-gateway/data
modem:
  adapter: qdc507
  tty: /dev/serial/by-id/usb-BAIWANG_Baiwang-if02-port0   # 稳定 by-id 路径（勿用 ttyUSB2 裸名）
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
  listen: 0.0.0.0:5060   # SIP 监听所有网卡（tailnet 客户端经 ts.net 域名连入）；0.0.0.0 = 绑定地址, 非连接目标
  realm: cellbridge
  push_token: "<你的 YakPhone 推送 token>"      # [REDACTED in repo example]
  users:
    - username: "1001"
      password: "<你的 SIP 密码>"                # [REDACTED in repo example]
webrtc:
  remote_ice_policy: relay
  local_ice_policy: all
  media_network_policy: tailnet-turn
  turn_credential_ttl: 10m
  private_turn:
    enabled: false           # ← 第一阶段禁用 TURN（tailnet 直连）
    host: ""
    port: 3478
    bind_ip: ""
push:
  mode: broker
  broker_url: https://push.example.com    # 旧 APNs 通道占位；实际短信/来电推送走 push.yakteam.com
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

**SIP 参数基线（勿乱改）：**

| 参数 | 值 | 说明 |
|---|---|---|
| 域名 | `<你的NAS>.ts.net` | tailnet MagicDNS |
| 端口 | 5060/UDP | 第一阶段不用 TLS |
| 账号 | `1001` / realm `cellbridge` | Digest 认证 |
| Codec | PCMU (PCMA=0) / 8000 Hz / mono | baresip 严格解析，2xx 必须带 Contact+Allow |
| SDP | `c=IN IP4 <tailnetIP>` | RTP 目的=INVITE 源 IP + SDP 端口（**无视 SDP c= 行 IP**，YakPhone 会写公网 IP） |
| TURN/STUN/SRTP | 全部留空/关闭 | tailnet 直连无 NAT |

---

## 7. YakPhone 客户端配置

1. App Store 安装 **Yak – AI SIP Phone**（baresip 内核）：
   👉 [https://apps.apple.com/in/app/yak-ai-sip-phone/id6763033863](https://apps.apple.com/in/app/yak-ai-sip-phone/id6763033863)（免费）
2. **获取 PushKit token（配置 NAS 前先做这一步）**：
   YakPhone → 设置（Settings）→ 账号/推送（Push）页 → **复制 Push Token**（一串 Base64 字符，形如 `AAA...==`）→ 填到 NAS `config.yaml` 的 `sip.push_token`。
   > 若 App 内找不到该页：在 Yak 设置里找 "PushKit" 或 "APNs Token" 字样；不同版本入口名称可能略有差异。
3. 账号设置：
   - **服务器**：`<你的NAS>.ts.net:5060`
   - **用户名**：`1001`；**密码**：与 config 一致
   - **传输**：UDP
   - **Codec 顺序**：PCMU（或 G711u）优先；第一阶段**不要用 Opus**
4. 注册成功后拨号界面出现"已注册"状态。
5. 短信：在 YakPhone 会话/消息界面输入**完整手机号**（如 `13800138000`）和内容发送——YakPhone 走 **SIP MESSAGE** 到 NAS，由 NAS 转 AT+CMGS 真发。

> 若拨号/短信按钮无反应：确认 NAS 侧 `journalctl -u cellbridge-gateway -f` 能看到 `sip register user=1001`；看不到说明注册没到 NAS（排查网段/防火墙）。

---

## 8. 语音链路（拨出/来电）

### 8.1 拨出（outbound）最终状态机

```text
YakPhone → INVITE (sip:10010@domain)
NAS: 100 Trying
NAS: Dial() → ① PrepareRoute: 每通电话前强制 rotating（killall mavo → 重建会话，绕过 UAC "aplay 触碰后 capture 死"固件缺陷）
      ② ATD10010; ③ WaitActive: 轮询 AT+CLCC 直到 state=0 (active)
NAS: 180 Ringing (Contact: <sip:cellbridge@IP>)
NAS: 蜂窝接通 → AwaitBridge(queue):
      ensureRuntime（短路线由已备）→ ALSAAudio.Start（capture 先开，aplay 后开）
NAS: 200 OK (Contact + Allow + SDP c=tailnetIP)
YakPhone: ACK（NAS 不回包，丢弃）
双向 RTP：PCMU/8000，NAS rtp=INVITE源IP+SDP端口，seq++/ts+=160
BYE → 200 OK → Hangup()：清 bridge（杀 arecord/aplay）+ modem 线路（15s 有界 ATH）
```

**关键实现细节（勿回退）：**
- **RTP 序号/时间戳必须递增**：`seq++`、`ts+=160`（每 20ms PCMU 帧）。恒 0 会被 baresip 按 RFC3550 判重复包丢弃 → 无声。
- **ACK 不回 200**：ACK 是 INVITE 事务收尾，回 200 会让 YakPhone 状态机卡 ringing。
- **200 OK 必带 `Contact` + `Allow`**：`<sip:cellbridge@IP>` + `Allow: INVITE, ACK, BYE, CANCEL, OPTIONS`；缺 Contact → baresip 报 badmessage。
- **modem ended 事件清 active**：NO CARRIER/未接来电 → 清 `active` 标志，否则下通必 500 "active modem call exists"。
- **ALSA 启动时机**：**必须等蜂窝接通（CLCC active）后才开 arecord/aplay**。拨号瞬间开 = UAC 未流化 → 永久全零。capture 先开、aplay 后开。
- **串口防死锁**：Dial/Hangup 的 AT Exchange 一律 15s 有界超时，超时自动释放 ioMu 锁（否则一次 ATH 卡死 = 之后所有拨号全卡）。
- **Call-ID 会话键 + 重传去重**：INVITE 重传时按 Call-ID 命中已有会话，回 180/200，不重复拨号。

### 8.2 来电（inbound）

```text
模块: RING / +CLIP（AT+CLIP=1、CCWA=1 需预置）
NAS HandleURC → incoming 事件（peer=主叫号码）
NAS ringClients: 向所有已注册 SIP 账号发 INVITE（带 SDP）
  + 同时 POST push.yakteam.com (type=voip, caller_uri, caller_name)
YakPhone 振铃（CallKit）→ 接听 → ACK（NAS 丢弃）+ RTP 双向
NO CARRIER / BYE → 清 active + 释放媒体
```

### 8.3 语音路由管理（QDC507）

每通电话前（`Dial()` 内 `PrepareRoute`）：

```text
1. killall -9 mavo-pcm-bridge.armv7 （⚠️ 必须 -9 + 验证清零：[m]avo 正则防 pgrep 自匹配）
2. 如无 mavo 残留：adb push 运行时（qdc507_aprv3.ko/qdc507_voice.ko/mavo-pcm-bridge）
   到模块 /run/maccellular-call/（sbc hash 校验）
3. insmod aprv3 → voice（幂等：test -d /sys/module/xxx || insmod）
4. alsaucm 校准（VoLTE verb + AuxPcm Rx/Tx 使能，日志 "VoLTE enable 1"）
5. sh /data/celldock-route-launch.sh 拉起 mavo（nohup + pid 文件 /run/celldock-voice-route.pid）
6. 轮询 routeReadyScript（pid 文件/进程/日志 "VoLTE route session active on hw:0,4"/audio_enable/hw:0,4 RUNNING）直到 "ready"
7. echo 1 > /sys/class/android_usb/f_audio/audio_enable
```

> ⚠️ **每通 rotating 是关键**：QDC507 UAC 固件的 capture 流在 arecord/aplay 触碰一次后会静音，只有**重建 mavo 会话**能恢复。实测：例行 rotating → 连续多通 nonzero=250 满血。

---

## 9. 短信链路（收发）

### 9.1 发送（YakPhone → SIP MESSAGE → AT+CMGS）

```text
YakPhone: MESSAGE sip:13800138000@domain SIP/2.0
          Content-Type: text/plain
          <正文>
NAS: handleMessageRequest:
     解析 Request-URI 号码 + 正文 → SMSEngine.Send(ctx, to, body)
     → adapter.SendSMS:
        ≤6 位短号（如 10010）→ AT+CMGF=1 + AT+CMGW/CMGS="短号" 文本模式
        >6 位长号         → AT+CMGF=0 + PDU（AT+CMGS=<tpdu长度> + PDU + 0x1A）
NAS → 200 OK
```

**SMS PDU 样例**（长号 185...）：`0001000B818155118865F50008044F60597D`（"你好"）。短号走文本模式避开 PDU `>` 提示符被 tty 抢占的问题。

**HTTP API 发送**（等价于 MESSAGE，供脚本/集成用；在 NAS 本机执行时用 127.0.0.1，远程统一走 tailnet 域名）：

```bash
curl -X POST http://127.0.0.1:8787/api/v1/messages \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <device-token>" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"to":"13800138000","body":"测试"}'
```

### 9.2 接收（SIM → +CMTI → 入库 → 推送）

```text
模块 +CMTI: "ME",<idx> → NAS AT+CMGL 拉取 → 解码（gsm7/ucs2）→ 入库 messages 表
→ sendMessagePush():
   YakPush(type=message, caller_uri=sip:<发件人>, message_body=<正文>)  ← push.yakteam.com
   （旧 APNs broker 未配置时忽略，不影响）
→ YakPhone 收到通知，打开即见短信
```

### 9.3 话费保护（重要！）

- 代码内置 **dry-run 开关**：`CELLBRIDGE_SMS_DRY_RUN` 环境变量，**默认 true**。
- dry-run=true：短信走 `AT+CMGW`（仅存储到模块 ME，**不发送、不计费**）。
- dry-run=false：走 `AT+CMGS`（真发，按运营商计费）。
- systemd 中开启真发：在 `[Service]` 加 `Environment=CELLBRIDGE_SMS_DRY_RUN=false`。
- 自测/复刻请**先保持 dry-run**，确认链路后再按需开启。

---

## 10. PushKit 推送（来电/短信唤醒）

YakPhone 官方推送端点（用户提供、仓库中提供参数格式）：

```http
POST https://push.yakteam.com/v1/notify
Content-Type: application/json

{
  "token": "<你的 PushKit token>",
  "caller_uri": "sip:1000@pbx.example.com",
  "caller_name": "1000",
  "type": "voip" | "missed" | "message",
  "message_body": "短信内容（type=message 时）"
}
```

- **来电**：RING/+CLIP 事件时发 `type=voip` → YakPhone PushKit 唤醒，App 未开也能振铃。
- **未接**：NO CARRIER 后发 `type=missed`（可选）。
- **短信**：入库后发 `type=message` + `message_body` → YakPhone 通知栏出现短信。
- token 从 config `sip.push_token` 读入；测试验证：`journalctl | grep yakpush` 应见 `yakpush sent type=message status="200 OK"`。

---

## 11. 自测脚本

仓库 `tools/` 提供（Mac 与 NAS 均可运行，纯 Python 无依赖）：

| 脚本 | 作用 | 关键输出 |
|---|---|---|
| `sip_fresh.py` | 全链路自测：REGISTER(digest) → INVITE(新 Call-ID) → 200 → BYE | `200 OK` |
| `sip_bye2.py` | 发起呼叫后立即 BYE（10s 窗口） | BYE→200 |
| `sip_dump200.py` | 打印 200 OK 原文（验证 Contact/Allow/SDP） | 响应头 |
| `readycheck.sh` | 模块语音路由 READY 逐项检查（pid/mavo/日志/audio_enable/hw RUNNING） | 每项 OK |
| `mavotest.sh` | 手跑 mavo 启动脚本，验证 pid 文件写入 | `1156 7011235` 格式 |

**语音心跳验证**（每通电话后看 NAS 日志）：

```text
voice cellular stats call_id=xxx frames=250 peak_max=32768 nonzero=250 mean=9000+
```

- `nonzero=250` 表示 250/250 样本非零 = **真实语音**；
- `nonzero=0` 或恒 `peak_max=32768` 无 nonzero = **静音/饱和**，按 §12 排查。

---

## 12. 八层排查方法论（§36）

遇到"无声/单通/拨号失败"按层从下往上查，**每层有明确证据才算过**：

```text
Layer 1  QDC507 固件: UAC=1（usbcfg 最后一位），AT+CFUN=1,1 生效
Layer 2  ALSA: /proc/asound/card0/pcm0c/sub0/status = RUNNING
         （通话中抓 arecord io：wchar 应持续增长，冻结=X RUN/死管道）
Layer 3  PCM 峰值: NAS 手动 arecord 通话中应 peak 32768，nonzero 过半
Layer 4  SIP: INVITE → 100/180 → 200 OK → ACK（重传按 Call-ID 去重）
Layer 5  RTP: 双向包计数（tcpdump udp port 40000-40100 或 gateway stats）
Layer 6  Codec: PCMU/8000/mono（勿 Opus）
Layer 7  YakPhone: 注册状态、拨号/接听按钮、扬声器
Layer 8  iOS: CallKit 是否接管、Audio Session route
```

**经典量化命令**：

```bash
# 1. ALSA 状态
cat /proc/asound/card0/pcm0c/sub0/status
# 2. 手动 capture 抓真实语音（gateway 停时）
timeout 10 arecord -q -D hw:0,0 -t raw -f S16_LE -r 8000 -c 1 -d 8 /tmp/x.raw
python3 -c "import struct;d=open('/tmp/x.raw','rb').read();s=struct.unpack('<%dh'%(len(d)//2),d[:len(d)//2*2]);print('peak',max(abs(int(x)) for x in s),'nonzero',sum(1 for x in s if x!=0))"
# 3. 网关统计（每 250 帧即 5s 一条）
journalctl -u cellbridge-gateway -f | grep 'voice cellular stats'
```

---

## 13. 已知坑与规避（踩坑实录）

| # | 现象 | 根因 | 解法（已内置代码） |
|---|---|---|---|
| 1 | 拨打 500 错误 | 200 OK 缺 `Contact`；baresip 严格解析报 badmessage | 2xx 必带 Contact+Allow |
| 2 | 挂断后重拨 500 | 未接来电 NO CARRIER 不清 modem active 标志 | ended 类 URC 自动清 active |
| 3 | 接通但完全无声 | RTP 序号/时间戳恒 0 → baresip 判重复包全丢 | media.go seq++ / ts+=160 |
| 4 | 呼叫到达 180 后永久振铃 | ACK 被回 200 → YakPhone 状态机卡 ringing | ACK 直接丢弃 |
| 5 | 拨号卡死（之后全卡） | 22:39 BYE 的 ATH Exchange 无超时永久占 ioMu 串口锁 | Dial/Hangup 15s 有界超时 |
| 6 | arecord 开在拨号瞬间→永久全零 | UAC gadget 未流化；"通话前开 capture"实验证实 | WaitActive（CLCC）等接通后才开 pair |
| 7 | 第一通有声，之后全无声 | **UAC capture 被 aplay 触碰一次即静音**（固件缺陷）；多 mavo 残留互踩 audio_enable | **每通 rotating**：killall -9 + [m]avo pgrep 防自匹配 + 重建单 mavo 会话 |
| 8 | `survivors=1` 无限重试 → rotating 卡死 → 拨号失败 | `pgrep -f mavo-pcm-bridge` 匹配到执行 shell 自身 | `pgrep -f [m]avo-pcm-bridge.armv7` |
| 9 | 模块冷复位后所有通话无声 | 重启后 ko 未重载、prepared 缓存骗过检查 | routeAlive 实测 + 全量 bootstrap 自愈 |
| 10 | pid 文件写不出 → READY 永不通过 | adb shell 内联脚本引号跨层损坏（`case \"$pid` 四重转义） | base64 写文件再 `sh` 执行 |
| 11 | `hw:0,4 busy errno 16` | 野 mavo 占 VoLTE PCM | stopRemoteRoute 直接 killall，不依赖 pid 文件 |
| 12 | readLoop 只读一帧就死 | bridge 绑了 AwaitBridge 的 answerCtx（defer cancel 即杀） | bridge 用 session 级 ctx |
| 13 | 重启 gateway 偶发 panic | Close() 与 URC 竞态：send on closed channel | HandleURC recover 兜底 |
| 14 | mavo 进程杀不死 | 老进程 D 状态/adb 环境 | killall -9 + 验证循环 |
| 15 | 短信误发烧话费 | 代码默认 AT+CMGS 真发 | `CELLBRIDGE_SMS_DRY_RUN` 默认 true（CMGW） |
| 16 | 短号短信 PDU 卡 `>` 提示符 | 输入提示符被 tty 抢占 | ≤6 位短号走 AT+CMGF=1 文本模式 |
| 17 | SDP c= 写公网 IP → RTP 发去公网回不来 | YakPhone SDP 使用家宽出口地址 | RTP 目的=INVITE 源 IP+SDP 端口 |
| 18 | 振铃中 180 后无 200 | 多轮叠加：路由回收时机/pid 转义/killall 残留/routePrepared 短路 | 前述 7/9/10 组合修复 |
| 19 | 通话中"断断续续"（3-7 秒静音+突发补音） | iPhone RTP 经 Tailscale 的秒级毛刺 → aplay 瞬时饥饿 → **UAC ADAPTIVE 播放时钟停摆 → ASYNC 下行 capture 被节流**（50 帧窗口耗时 3-7 秒） | **上行 jitter buffer**：RTP 帧进 50 帧环形队列，固定 20ms 节拍器平滑写入 aplay；队列空写静音防 XRUN，满则丢最旧帧限延迟（bridge.go） |
| 20 | 蜂窝方向偶发丢帧（帧率掉到 ~19fps） | UAC ASYNC capture 抖动超 ALSA 默认缓冲 | arecord/aplay 加 `--buffer-size=8192 --period-size=1024`（chan-quectel 同参数，60s 实测零丢帧） |

---

## 14. 安全与话费保护

- **默认 dry-run**：不开 `CELLBRIDGE_SMS_DRY_RUN=false` 就永远只存不发，杜绝误发短信扣费。
- **token/密码**：仓库示例一律 `[REDACTED]`；`push_token`、SIP 密码、device token 由用户自填，不入库不入仓库。
- **避免一刀切**：SIP 注册仍允许任意已配置账号；API 需 bearer token（设备配对生成）。
- 日志 `redact_logs: true` 打开，敏感字段自动打码。

---

## 15. 开源信息与法律

- **仅开源可复刻部分**：gateway 源码、infra 示例配置、tools 自测脚本、本文档。
- **不随仓库分发**：QDC507 模块侧运行时（qdc507_aprv3.ko / qdc507_voice.ko / mavo-pcm-bridge / alsaucm）——这些是模块厂商闭源二进制，用户需自行获得（参考 DJOneHub 项目与模块附带的固件材料）。
- 参考项目：https://github.com/ZenGeekLabs/DJOneHub（DJI 一代 4G 模块 macOS 管理工具，USB 通道/eSIM 实现可参照）；https://github.com/AmorXxx/iPhone_air_esim_tutorial（EC20 系 UAC+asterisk-chan-quectel 短信/语音桥接思路）。
- License：见仓库 LICENSE（建议 MIT for 源码；文档 CC-BY-4.0）。