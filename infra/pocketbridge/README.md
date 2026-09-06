# PocketBridge

PocketBridge 与 NAS 共用 `cellbridge-gateway`、SQLite、REST/WSS 和 WebRTC 协议；
硬件只负责把 USB modem 暴露给 Linux，并通过 USB Ethernet 或 Wi-Fi 提供普通 IP
链路。iPhone 不直接驱动 DJI/Quectel 私有 USB interface。

## 本地服务约定

- Gateway 控制面只监听 bridge 本机或内网地址。
- Caddy 为本地 HTTPS 提供证书，Bonjour 服务类型为 `_cellbridge._tcp`。
- iOS 发现服务后只把解析出的 HTTPS URL 作为候选路由，不接管 iPhone 默认路由。
- `remote_ice_policy` 仍固定为 `relay`；PocketBridge 的本地媒体请求使用 `pocket`
  transport，优先 direct ICE。

`avahi/cellbridge.service` 是可复制到 Avahi 的服务描述模板。证书签发、USB gadget
网络和具体 SBC 型号属于硬件集成工作，不应改变 Gateway API。
