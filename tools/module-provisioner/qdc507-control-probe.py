#!/usr/bin/env python3
"""Read-only v8 H1 probe for a QDC507 ADB shell.

The probe deliberately does not call ``adb root`` and never writes to the
module. It records the currently attached ADB state and, only when a device
is already available, reads the module's kernel, mounts, device nodes,
processes, sockets and network metadata required by the v8 M0 decision.
"""

from __future__ import annotations

import argparse
import json
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path


READ_COMMANDS = {
    "uname": "uname -a",
    "proc_cmdline": "cat /proc/cmdline",
    "mounts": "mount",
    "disk_free": "df -k",
    "processes": "ps",
    "dev": "ls -l /dev",
    "dev_smd": "ls -l /dev/smd*",
    "dev_tty": "ls -l /dev/tty*",
    "dev_diag": "ls -l /dev/diag*",
    "dev_qmi": "ls -l /dev/qmi*",
    "dev_cdc": "ls -l /dev/cdc*",
    "sys_class": "ls -l /sys/class",
    "proc_devices": "cat /proc/devices",
    "proc_misc": "cat /proc/misc",
    "process_candidates": "ps | grep -E 'rild|qmuxd|netmgrd|qmi|ims|voice|at|modem' | grep -v grep",
    "unix_sockets": "cat /proc/net/unix",
    "netstat": "command -v ss >/dev/null && ss -lntup || command -v netstat >/dev/null && netstat -lntup || true",
}


def run(command: list[str], timeout: float = 8.0) -> tuple[int, str, str]:
    try:
        completed = subprocess.run(
            command,
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except FileNotFoundError as error:
        return 127, "", str(error)
    except subprocess.TimeoutExpired as error:
        return 124, error.stdout or "", "command timed out"
    return completed.returncode, completed.stdout, completed.stderr


def trim(value: str, limit: int = 16_384) -> str:
    value = value.replace("\x00", "")
    if len(value) <= limit:
        return value
    return value[:limit] + "\n...[truncated]"


def adb_command(
    adb: str,
    serial: str | None,
    transport_id: str | None,
    *args: str,
) -> list[str]:
    command = [adb]
    if serial:
        command += ["-s", serial]
    elif transport_id:
        command += ["-t", transport_id]
    return command + list(args)


def probe(adb: str, serial: str | None, transport_id: str | None = None) -> dict[str, object]:
    result: dict[str, object] = {
        "schema": "cellbridge.v8.module-control-probe.v1",
        "capturedAt": datetime.now(timezone.utc).isoformat(),
        "readOnly": True,
        "adbPath": shutil.which(adb) or adb,
        "serial": serial,
        "transportId": transport_id,
        "status": "UNKNOWN",
        "deviceAttached": False,
        "adbDevices": "",
        "shellIdentity": "",
        "shellIsRoot": False,
        "commands": {},
    }

    if shutil.which(adb) is None and not Path(adb).exists():
        result["status"] = "BLOCKED_ADB_NOT_FOUND"
        result["error"] = f"adb executable not found: {adb}"
        return result

    # Discovery itself is deliberately unselected. The selected serial or
    # transport is applied only to state/shell reads after the inventory is
    # captured, so the evidence retains the complete ADB view.
    devices_rc, devices_out, devices_err = run(adb_command(adb, None, None, "devices", "-l"))
    result["adbDevices"] = trim(devices_out or devices_err)
    if devices_rc != 0:
        result["status"] = "BLOCKED_ADB_UNAVAILABLE"
        result["error"] = trim(devices_err or devices_out)
        return result

    state_rc, state_out, state_err = run(adb_command(adb, serial, transport_id, "get-state"))
    state = (state_out or state_err).strip()
    result["adbState"] = state
    if state != "device":
        normalized_state = state.lower()
        if not state or "no devices/emulators found" in normalized_state:
            result["status"] = "BLOCKED_NO_ADB_DEVICE"
        else:
            result["status"] = f"BLOCKED_ADB_{state.upper().replace(' ', '_')}"
        return result

    result["deviceAttached"] = True
    identity_rc, identity_out, identity_err = run(adb_command(adb, serial, transport_id, "shell", "id"))
    identity = (identity_out or identity_err).strip()
    result["shellIdentity"] = identity
    result["shellIsRoot"] = "uid=0" in identity
    if identity_rc != 0:
        result["status"] = "BLOCKED_ADB_SHELL"
        result["error"] = trim(identity_err or identity_out)
        return result

    if not result["shellIsRoot"]:
        result["status"] = "BLOCKED_NON_ROOT_SHELL"
        result["detail"] = "v8 M0 probe requires a verified root shell; no adb root escalation was attempted"
        return result

    commands: dict[str, object] = {}
    for name, shell_command in READ_COMMANDS.items():
        rc, out, err = run(adb_command(adb, serial, transport_id, "shell", "sh", "-c", shell_command))
        commands[name] = {
            "command": shell_command,
            "returnCode": rc,
            "stdout": trim(out),
            "stderr": trim(err),
        }
    result["commands"] = commands
    result["status"] = "H1_M0_READ_ONLY_CAPTURED"
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adb", default="adb", help="adb executable")
    selector = parser.add_mutually_exclusive_group()
    selector.add_argument("--serial", help="adb device serial, if more than one is attached")
    selector.add_argument("--transport-id", help="adb physical transport_id, preferred for serial-less QDC507")
    parser.add_argument("--output", type=Path, required=True, help="JSON evidence output path")
    args = parser.parse_args()

    evidence = probe(args.adb, args.serial, args.transport_id)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(evidence, ensure_ascii=False, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps({"status": evidence["status"], "output": str(args.output)}, ensure_ascii=False))
    return 0 if str(evidence["status"]).startswith("H1_") else 2


if __name__ == "__main__":
    sys.exit(main())
