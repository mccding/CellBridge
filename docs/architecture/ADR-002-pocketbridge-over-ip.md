# ADR-002：PocketBridge 使用标准 IP 链路

状态：已接受

PocketBridge 由 Linux SBC 承担 modem 驱动和 Gateway 运行时；iPhone 只连接 HTTPS/WSS/WebRTC 服务。优先使用 USB Ethernet 或本地 Wi-Fi，Bonjour 服务为 `_cellbridge._tcp.local`，并保留 link-local fallback。

