#!/usr/bin/env python3
"""
SIP full-chain self-test for the CellBridge gateway.

Usage:
  SIP_DOMAIN=cellbridge-nas.example.ts.net \
  SIP_USER=1001 SIP_PASS=yourpass \
  python3 sip_fresh.py [dest] [hold_seconds]

Flow: REGISTER (digest) -> INVITE (fresh Call-ID) -> 200 OK -> BYE.
Dest defaults to 10010 (free operator hotline). Real call, use at your own cost.
"""
import hashlib
import os
import re
import socket
import sys
import time

DOMAIN = os.environ.get("SIP_DOMAIN", "cellbridge-nas.example.ts.net")
USER = os.environ.get("SIP_USER", "1001")
PASSWORD = os.environ.get("SIP_PASS", "")
SERVER = ("127.0.0.1", 5060)          # run on the NAS itself
LOCAL = ("127.0.0.1", 5092)

DEST = sys.argv[1] if len(sys.argv) > 1 else "10010"
HOLD = float(sys.argv[2]) if len(sys.argv) > 2 else 6.0

s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(4)


def register():
    msg = (
        f"REGISTER sip:{DOMAIN} SIP/2.0\r\n"
        f"Via: SIP/2.0/UDP {LOCAL[0]}:{LOCAL[1]};branch=z9hG4bKr1;rport\r\n"
        f"From: <sip:{USER}@{DOMAIN}>;tag=t9\r\n"
        f"To: <sip:{USER}@{DOMAIN}>\r\n"
        f"Call-ID: rg-uuid-1\r\nCSeq: 1 REGISTER\r\n"
        f"Contact: <sip:{USER}@{LOCAL[0]}:{LOCAL[1]}>\r\nExpires: 300\r\n"
        "Content-Length: 0\r\n\r\n"
    )
    s.sendto(msg.encode(), SERVER)
    r = s.recvfrom(8192)[0].decode()
    if "401" not in r:
        print("REGISTER:", r.split("\r\n")[0], "(no challenge?)")
        return
    nonce = re.search(r'nonce="([^"]+)"', r).group(1)
    uri = f"sip:{DOMAIN}"
    ha1 = hashlib.md5(f"{USER}:cellbridge:{PASSWORD}".encode()).hexdigest()
    ha2 = hashlib.md5(f"REGISTER:{uri}".encode()).hexdigest()
    resp = hashlib.md5(f"{ha1}:{nonce}:{ha2}".encode()).hexdigest()
    auth = (
        f'Authorization: Digest username="{USER}", realm="cellbridge", '
        f'nonce="{nonce}", uri="{uri}", response="{resp}", algorithm=MD5\r\n'
    )
    s.sendto(("REGISTER sip:%s SIP/2.0\r\nVia: SIP/2.0/UDP %s:%d;branch=z9hG4bKr2;rport\r\n"
              f"From: <sip:{USER}@{DOMAIN}>;tag=t9\r\nTo: <sip:{USER}@{DOMAIN}>\r\n"
              "Call-ID: rg-uuid-1\r\nCSeq: 2 REGISTER\r\n"
              f"Contact: <sip:{USER}@{LOCAL[0]}:{LOCAL[1]}>\r\nExpires: 300\r\n" + auth +
              "Content-Length: 0\r\n\r\n").format(DOMAIN, LOCAL[0], LOCAL[1]).encode(), SERVER)
    print("REGISTER:", s.recvfrom(8192)[0].decode().split("\r\n")[0])


def call():
    call_id = "fresh-%d" % int(time.time())
    branch = "z9hG4bKfresh"
    sdp = (
        "v=0\r\no=t 0 0 IN IP4 127.0.0.1\r\ns=t\r\nc=IN IP4 127.0.0.1\r\n"
        f"t=0 0\r\nm=audio 40082 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n"
    )
    invite = (
        f"INVITE sip:{DEST}@{DOMAIN} SIP/2.0\r\n"
        f"Via: SIP/2.0/UDP {LOCAL[0]}:{LOCAL[1]};branch={branch};rport\r\n"
        f"From: <sip:{USER}@{DOMAIN}>;tag=t1\r\n"
        f"To: <sip:{DEST}@{DOMAIN}>\r\nCall-ID: {call_id}\r\nCSeq: 1 INVITE\r\n"
        f"Contact: <sip:{USER}@{LOCAL[0]}:{LOCAL[1]}>\r\n"
        "Content-Type: application/sdp\r\n"
        f"Content-Length: {len(sdp)}\r\n\r\n{sdp}"
    )
    t0 = time.time()
    s.sendto(invite.encode(), SERVER)
    ok = False
    while time.time() - t0 < 60:
        try:
            line = s.recvfrom(8192)[0].decode().split("\r\n")[0]
            print("GOT:", line)
            if "200 OK" in line:
                ok = True
                break
        except socket.timeout:
            continue
    time.sleep(HOLD)
    bye = (
        f"BYE sip:{DEST}@{DOMAIN} SIP/2.0\r\n"
        f"Via: SIP/2.0/UDP {LOCAL[0]}:{LOCAL[1]};branch={branch}b;rport\r\n"
        f"From: <sip:{USER}@{DOMAIN}>;tag=t1\r\n"
        f"To: <sip:{DEST}@{DOMAIN}>\r\nCall-ID: {call_id}\r\nCSeq: 2 BYE\r\n"
        "Content-Length: 0\r\n\r\n"
    )
    s.sendto(bye.encode(), SERVER)
    print("CALL:", "OK" if ok else "FAIL (no 200)")

if __name__ == "__main__":
    if not PASSWORD:
        print("Set SIP_PASS env var"); sys.exit(2)
    register()
    call()