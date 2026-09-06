# ADR-001：NAS-first，PocketBridge 共用 Gateway Core

状态：已接受

## 决策

CellBridge V1 以 Linux NAS + USB 蜂窝模块为主部署形态。PocketBridge 通过标准 IP 链路接入 iOS，运行同一套 Gateway Core，不让 iOS 直接驱动裸 modem USB interface。

## 原因

iOS App 不提供面向普通 iPhone App 的通用 USBDriverKit 路径；ExternalAccessory 需要 MFi/iAP。Linux 适合处理 USB serial、AT、音频和设备重枚举。

## 影响

硬件和网络发现可以替换，但 SMS、电话状态机、API v1、数据库和 WebRTC 业务接口必须共用。

