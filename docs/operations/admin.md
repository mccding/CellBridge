# CellBridge 运维命令

Gateway 进程只提供业务 API；需要导出诊断、备份或恢复时使用同版本的
`cellbridge-admin`，并先停止 Gateway，避免恢复时仍有 SQLite 写入。

## 诊断包

```bash
cellbridge-admin diagnostics export \
  --data-dir /var/lib/cellbridge \
  --config /etc/cellbridge/config.yaml \
  --output /tmp/cellbridge-diagnostics.zip
```

包内固定包含 `version.json`、只读 `probe.json`、数据库健康摘要、脱敏网络信息、
TURN 检查占位结果、空的脱敏日志占位文件和脱敏配置。不会导出短信正文、token、
APNs token、IMSI/ICCID 或原始日志。

## 备份与恢复

```bash
cellbridge-admin backup \
  --data-dir /var/lib/cellbridge \
  --config /etc/cellbridge/config.yaml \
  --output /secure/cellbridge-backup.zip

systemctl stop cellbridge
cellbridge-admin restore \
  --data-dir /var/lib/cellbridge \
  --input /secure/cellbridge-backup.zip \
  --confirm-restore
systemctl start cellbridge
```

备份使用 SQLite 一致性快照，默认只包含公有配置、数据库和设备公钥；录音必须显式
使用 `--include-recordings`。恢复前会把现有数据库改名为带时间戳的
`.pre-restore-*.bak`，不会覆盖既有文件。

## 现场验收仍需执行

诊断命令不伪造硬件或公网结果。插入模块后仍需运行 `cellbridge-probe`，配置真实
TURN endpoint、用户名和临时 credential 后运行：

```bash
cellbridge-admin turn-test \
  --server turn:turn.example.com:3478 \
  --username "$TURN_USERNAME" \
  --credential "$TURN_CREDENTIAL"
```

再按 NAS 文档完成通话、短信和拔插恢复测试。TURN credential 不写入诊断包和普通日志。

## Push Broker

Gateway 的 `push.mode: broker` 配合 `push.broker_url` 启用可替换 Push Broker。Broker 需要实现：

- `POST /v1/push/voip`：接收 `callUUID`、`callId`、`handle`、`gatewayId`、`issuedAt`。
- `POST /v1/push/message`：接收 `messageId`、`gatewayId`、`issuedAt`，不接收短信正文。

Gateway 通过 `CELLBRIDGE_PUSH_BROKER_AUTHORIZATION` 注入认证 header。设备撤销后，设备 token 不会再被列入推送目标；Broker/APNs 的真实投递仍需使用部署方的 Apple Developer credentials 验收。

录音由 Gateway 保存为双向采集的 M4A/AAC-LC，并受最小剩余空间和保留策略约束。
V1 自动录音保持 `auto_mode: off`；手动录音必须由用户在通话中主动点击，录音文件不会
上传 Push Broker。自动录音只有在双方可听见提示音并完成真机验收后才能启用。

录音设置可通过已鉴权 API 查看和更新：

```text
GET /api/v1/settings/recording
PUT /api/v1/settings/recording
```

当前 PUT 允许调整是否启用、保留天数和最低剩余空间；自动录音模式会返回 `409`，
直到录音提示音链路完成认证。设置写入 Gateway SQLite，重启后保留。

## 设备撤销

设备撤销会立即使该设备的 access/refresh token 失效，也会停止向它发送 Push：

```bash
cellbridge-admin device-revoke \
  --data-dir /var/lib/cellbridge \
  --device-id dev_... \
  --confirm-revoke
```
