# CellBridge QDC507 module agent

This is the clean-room, module-local Agent described by v8. It is deliberately
standalone and uses only the Go standard library so it can be cross-compiled
as a static ARMv7 binary for the QDC507 Linux userspace.

The current implementation is an H2/H4 bring-up slice:

- binds HTTP only to an explicitly configured ECM address;
- refuses wildcard and loopback binds in production mode;
- advertises `_cellbridge-module._tcp` with only the v8-approved TXT keys;
- exposes typed identity, health, line, SMS and call endpoints;
- protects mutating endpoints and WebSocket events with a bearer token;
- keeps a bounded monotonic event journal;
- reads modem status and registration through the existing QDC507
  `qmi_simple_ril_test` console when explicitly enabled;
- supports dial/hangup only behind the explicit
  `CB_AGENT_ENABLE_VERIFIED_CALL=1` HIL switch, using a persistent QMI console
  session and radio-state truth.

It does not expose arbitrary AT/QMI/shell APIs. SMS, answer, DTMF and media
remain blocked until their own controlled HILs pass. The SMS HIL now waits for
the QMI terminal result and returns a verified error when the modem reports
`RESULT CODE 1`; the current binary therefore cannot claim successful SMS or
Full Voice completion.

## Safe local smoke run

```sh
CB_AGENT_BIND_ADDR=127.0.0.1:18788 \
CB_AGENT_ALLOW_LOOPBACK=1 \
CB_AGENT_DEVICE_ID=dev-smoke \
CB_AGENT_TOKEN=local-smoke-token \
go run ./cmd/cellbridge-agent
```

For the module, set `CB_AGENT_BIND_ADDR` to the actual ECM interface address
and do not set `CB_AGENT_ALLOW_LOOPBACK`. `CB_AGENT_SERVICE_IP`, if set, must
match that same ECM address; the HTTP and mDNS sockets both use that explicit
source address. No address is hard-coded.

## Cross build

```sh
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 \
  go build -trimpath -ldflags='-s -w' \
  -o ../artifacts/module-agent/cellbridge-agent-armv7 \
  ./cmd/cellbridge-agent
```

Installing the binary, choosing a persistent path, changing USB mode, or
adding startup hooks is intentionally a separate M1/M2 operation and is not
performed by this build.
