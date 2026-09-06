# NAS 部署

1. 在 NAS 上准备 Docker、Compose、`dialout` 组、Tailscale，以及稳定的蜂窝模块 USB 串口。
2. 将 Tailscale 安装在 NAS Host OS，登录正确 tailnet，启用 MagicDNS/HTTPS，记录
   `tailscale ip -4` 和 `tailscale status`。禁止启用 Funnel；`tailscale funnel status` 有
   活动配置即判定部署失败。
3. 复制 `infra/docker/.env.example` 为 `infra/docker/.env`，设置 `TAILSCALE_IP`、随机
   `TURN_AUTH_SECRET` 和实际配置路径。不要提交 `.env`。Gateway 使用 Host network 绑定
   `127.0.0.1:8787`，不配置公网域名、端口转发或公网 TURN。
4. 在仓库根目录执行安装脚本：

   ```sh
   ./infra/install.sh
   ```

   也可以直接执行等价的 `docker compose ... up -d --build` 命令。

5. 配置 Tailscale Serve：

   ```sh
   tailscale serve --bg http://127.0.0.1:8787
   tailscale serve status
   tailscale funnel status
   ```

   Serve 必须显示 `tailnet only` 并代理到 `127.0.0.1:8787`；Funnel 必须 disabled。iPhone
   安装官方 Tailscale、连接同一 tailnet，然后使用 `https://<nas-hostname>.<tailnet>.ts.net`
   配对 CellBridge。NAS 即使与 iPhone 在同一 Wi-Fi，也使用这个固定 Tailscale FQDN。
   Tailnet 模式下，配对请求只允许从 Tailscale Serve 进入；Serve 代理到本机回环
   `127.0.0.1:8787` 时，Gateway 看到回环来源也是预期行为。公网入口和 Funnel
   配对始终拒绝。

### fnOS Web 与 CellBridge 共存

fnOS 自带的 `trim` 包是系统核心，不能用 `apt remove` 卸载。为避免它的 nginx
抢占 CellBridge 的 Tailnet 443，本次 NAS 使用可回滚的网络隔离方式：

- `trim_nginx.service` 保持 masked；没有删除 `/usr/trim`、fnOS Web 文件或证书。
- `fnos-web-cellbridge.service` 在独立 network namespace 中启动原厂
  `/usr/trim/nginx/sbin/nginx`，所以 fnOS 的 wildcard 80/443/5666/5667 配置和
  Unix-socket 后端保持不变。
- Host 只把物理网卡 `eth0`/`usb0` 上的 80、443、5666、5667 转发到该 namespace；
  `tailscale0:443` 仍只由 Tailscale Serve 提供 CellBridge。
- 安装文件为 [`infra/fnos/fnos-web-cellbridge.sh`](../../infra/fnos/fnos-web-cellbridge.sh)
  和 [`infra/fnos/fnos-web-cellbridge.service`](../../infra/fnos/fnos-web-cellbridge.service)。

在 fnOS NAS 上部署这两个文件后执行：

```sh
systemctl mask --now trim_nginx.service
install -m 0755 /path/to/fnos-web-cellbridge.sh /usr/local/sbin/fnos-web-cellbridge
install -m 0644 /path/to/fnos-web-cellbridge.service /etc/systemd/system/fnos-web-cellbridge.service
systemctl daemon-reload
systemctl enable --now fnos-web-cellbridge.service
```

因此 fnOS Web 使用 `https://<NAS-LAN-IP>` 或原来的 `https://<NAS-LAN-IP>:5667`，
CellBridge 使用 Tailnet FQDN 的 443。不要直接对 fnOS 定制 nginx 执行 `nginx -t`：
该版本包含自恢复模块，会重建其配置目录；应通过上述 systemd 服务管理。回滚时先
停止并禁用 `fnos-web-cellbridge.service`，再恢复 `trim_nginx.service` 的原始 unit。

本次 ARM64 NAS 验证使用的是 Baiwang QDC507：稳定 AT 口为
`/dev/serial/by-id/usb-BAIWANG_Baiwang-if02-port0`，串口速率为 `9600`。
可复制 [`infra/config.nas-qdc507.example.yaml`](../../infra/config.nas-qdc507.example.yaml)
作为起始配置。该模块的 AT 能力包括 IMS/VoLTE、DTMF 和 QPCMV，硬件同时暴露 USB
Audio（`S16_LE`、单声道、8000 Hz）；仓库会将其报告为 `voice_control_only`。仓库已
提供 `alsa-uac` backend；只有外部模块侧语音运行时、PCM/UAC 路由和真实通话验收全部
通过后，才将配置改为 `voice.enabled: true`、`backend: qdc507`、`bootstrap: true`
并让 UI 显示完整语音。`bootstrap` 默认关闭，避免未完成 ABI/现场验证时自动加载模块。

Compose 同时需要把实际字符设备加入 Docker device cgroup；`CELLBRIDGE_MODEM_DEVICE`
默认是 `/dev/ttyUSB2`，只用于设备权限映射，应用配置仍使用上面的稳定 `by-id` 路径。
如果模块枚举到其他 `ttyUSB*`，在执行 Compose 前覆盖这个变量，并先用 `readlink -f`
确认它指向目标 AT 口。

如果 NAS 访问 `proxy.golang.org` 超时，可在构建时把 `GOPROXY` 指向可达的 Go 模块
代理，例如 `GOPROXY=https://goproxy.cn,direct`；Compose 会把该值传给 Gateway
镜像的构建阶段，不写入运行时配置。

录音默认写入 Gateway 数据目录下的 `recordings/`：整理期间的双轨临时 PCM 位于
`recordings/work/`，完成后的文件按 `YYYY/MM/<recording-id>.m4a` 保存。配置中的
`minimum_free_bytes` 默认是 500 MiB，`retention_days` 默认是 90 天，`auto_mode`
默认是 `off`。录音只能通过鉴权 API 访问；不要把 `recordings/` 目录通过 Caddy、NAS
文件分享或公开静态目录直接暴露。

Compose 使用 Host network，使 Gateway 真实绑定 Host loopback；ADB 固定在 `127.0.0.1:5037`，
Raw AT 不提供网络桥接，Voice Agent 使用 Unix socket。默认服务不执行任何会改写 USB identity、
IMEI、NV 或网络模式的 AT 命令。Tailscale Serve 是 CellBridge 的唯一业务入口；fnOS Web
仅用于物理 LAN/USB 管理入口。

镜像同时提供 `cellbridge-admin` 和 `cellbridge-probe`，现场可用
`docker compose run --rm --entrypoint /usr/local/bin/cellbridge-probe gateway --json` 做只读探测。

如果尚未准备好私有 TURN，可先只启动 Gateway 验证 overlay；它仍只绑定 loopback：

```bash
TAILSCALE_IP=127.0.0.1 \
TURN_AUTH_SECRET=validation-only \
TURN_REALM=cellbridge.invalid \
TARGETARCH=arm64 VERSION=validation \
CELLBRIDGE_CONFIG=/mnt/docker-compose/cellbridge-gateway/config.yaml \
docker compose -p cellbridge-validation \
  -f infra/docker/docker-compose.yml \
  -f infra/docker/docker-compose.nas-validation.yml \
  up -d --build gateway

curl http://127.0.0.1:18080/api/v1/health
docker compose -p cellbridge-validation logs --tail=100 gateway
```

验证结束只停止这个新项目即可；不要使用 `down -v`，以免删除 Gateway 数据卷：

```bash
docker compose -p cellbridge-validation stop gateway
```

## 备份与恢复

Gateway 数据位于 Docker volume `cellbridge-data`，其中包含 SQLite WAL 数据库。停服务后备份整个 volume；恢复时还原 volume，再执行 `docker compose ... up -d`。设备私钥和访问令牌不在 Gateway 数据库中，iOS 端 Keychain 需要单独备份/重新配对。

如需包含录音，使用 admin 的 `--include-recordings` 选项，并在 Gateway 停止后把整个
`recordings/` 目录一并纳入备份。恢复前会保留现有数据库的时间戳备份，不覆盖既有文件。

## 上线前验收

- `GET /api/v1/health` 返回 `status=ok`。
- Router CellBridge port forward 为 0；公网 443、3478、8787、5037 和 TURN relay range 均不可达。
- `tailscale serve status` 为 tailnet-only、目标为 `127.0.0.1:8787`；`tailscale funnel status` disabled。
- Gateway loopback、ADB loopback、Voice Agent Unix socket 通过 `cellbridge-admin security-probe`。
- Tailscale Grants 只允许普通 iPhone 访问 TCP 443/UDP-TCP 3478；SSH 仅管理员授权。
- 短信单条、GSM-7/UCS-2 长短信、乱序分片和重复 URC 均通过测试。
- 线路注册、SIM 状态、信号、来电、接听、挂断和 DTMF 在实体模块上逐项验收。
- NAS Mode 的 WebRTC 初始强制 `tailnet-turn` / relay-only；ICE credential 每次通话动态下发，App 不硬编码。
- 手动开始录音后，录音状态依次经过 `recording` → `finalizing` → `ready`；停止通话
  时即使录音整理失败，通话也必须正常结束。
- 播放器必须使用 Bearer 鉴权和 HTTP Range；删除后音频与波形接口均不可再访问。
- 用低磁盘空间、Gateway 重启、录音整理失败和保留期清理分别验收录音异常路径。
- APNs/PushKit/CallKit、私有 TURN、真实 PCM/UAC 和 30 分钟通话需要真实凭据与实体设备验收；公网域名和公网 TURN 不属于 v4 默认方案。
