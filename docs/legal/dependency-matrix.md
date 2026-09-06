# 依赖与许可证矩阵

本项目目前只包含自行实现的 Gateway probe、协议模型和 iOS 基础层，没有复制参考项目源代码，也没有把其预编译 runtime 或 helper 打进镜像。

| Name | Source URL | Version/commit | License | Copied code? | Modified? | Notice required? | Commercially compatible? |
|---|---|---|---|---|---|---|---|
| MacCellular | https://github.com/yuexiazhuojiu-byte/MacCellular | review before use | PolyForm Noncommercial 1.0.0（按计划记录） | no | no | review | no, without legal review |
| NasAnySim | https://github.com/mccding/NasAnySim | review before use | review repository assets separately | no | no | review | unknown |
| dji-4g-vohive-mac | https://github.com/wlzh/dji-4g-vohive-mac | review before use | review repository and upstream assets separately | no | no | review | unknown |
| Go standard library | https://go.dev/ | toolchain | BSD-style | no | no | no | yes |
| modernc.org/sqlite | https://gitlab.com/cznic/sqlite | v1.57.0 | BSD-3-Clause | no | no | retain license | yes, subject to license |
| Swift / SwiftUI / SwiftData | https://developer.apple.com/ | iOS 17 SDK | Apple SDK terms | no | no | Apple terms | subject to Apple terms |
| WebRTC Native XCFramework | https://webrtc.googlesource.com/src/ | add only after audit | audit at adoption time | no | no | yes | audit required |
| Pion WebRTC | https://github.com/pion/webrtc | add only after audit | audit at adoption time | no | no | yes | audit required |
| coturn | https://github.com/coturn/coturn | deployment dependency | BSD-style | no | no | retain license | yes, subject to license |
| Caddy | https://github.com/caddyserver/caddy | deployment dependency | Apache-2.0 | no | no | retain license | yes |
| gopkg.in/yaml.v3 | https://github.com/go-yaml/yaml | v3.0.1 | MIT | no | no | retain license | yes |
| Pion WebRTC | https://github.com/pion/webrtc | v4.2.19 | MIT | no | no | retain license | yes |

任何新增依赖都必须补齐版本、许可证、notice 和商业兼容性结论后才能进入 release 镜像或 App。
