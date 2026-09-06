# ADR-003：V1 使用 WebRTC + PCMU

状态：已接受

V1 实时媒体固定使用 PCMU / 8000 Hz / mono / payload type 0 / 20 ms frame。远程模式默认 relay-only 并通过 coturn；LAN/PocketBridge 允许 direct ICE。V1 信令使用 non-trickle ICE。

