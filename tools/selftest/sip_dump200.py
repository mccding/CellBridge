import socket, hashlib, re, time

s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(4)

def send(m):
    s.sendto(m.encode(), ("127.0.0.1", 5060))
    try:
        data,_=s.recvfrom(8192)
        return data.decode()
    except Exception as e:
        return "TIMEOUT "+str(e)

uri="sip:YOURNAS.ts.net"
def reg_msg(cseq, branch, callid, extra=""):
    return ("REGISTER "+uri+" SIP/2.0\r\n"
      "Via: SIP/2.0/UDP 127.0.0.1:5090;branch="+branch+";rport\r\n"
      "From: <sip:1001@YOURNAS.ts.net>;tag=self1\r\n"
      "To: <sip:1001@YOURNAS.ts.net>\r\n"
      "Call-ID: "+callid+"@127.0.0.1\r\n"
      "CSeq: "+cseq+" REGISTER\r\n"
      "Contact: <sip:1001@127.0.0.1:5090>\r\n"
      "Expires: 3600\r\n"+extra+
      "Content-Length: 0\r\n\r\n")

r=send(reg_msg("1","z9hG4bKself1","selfregA"))
if "401" in r:
    nonce=re.search(r'nonce="([^"]+)"', r).group(1)
    import os
user = os.environ.get("SIP_USER", "1001")
pw = os.environ.get("SIP_PASS", "")
    ha1=hashlib.md5((user+":"+realm+":"+pw).encode()).hexdigest()
    ha2=hashlib.md5(("REGISTER:"+uri).encode()).hexdigest()
    resp=hashlib.md5((ha1+":"+nonce+":"+ha2).encode()).hexdigest()
    auth='Authorization: Digest username="1001", realm="cellbridge", nonce="'+nonce+'", uri="'+uri+'", response="'+resp+'", algorithm=MD5\r\n'
    r2=send(reg_msg("2","z9hG4bKself2","selfregB",auth))
    print("REG2:", r2.split("\r\n")[0])

invite=("INVITE sip:10010@YOURNAS.ts.net SIP/2.0\r\n"
  "Via: SIP/2.0/UDP 127.0.0.1:5090;branch=z9hG4bKself3;rport\r\n"
  "From: <sip:1001@YOURNAS.ts.net>;tag=selfinv2\r\n"
  "To: <sip:10010@YOURNAS.ts.net>\r\n"
  "Call-ID: selfinv2@127.0.0.1\r\n"
  "CSeq: 1 INVITE\r\n"
  "Contact: <sip:1001@127.0.0.1:5090>\r\n"
  "Content-Type: application/sdp\r\n"
  "Content-Length: 143\r\n\r\n"
  "v=0\r\no=self 0 0 IN IP4 127.0.0.1\r\ns=self\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 40002 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
body=invite.split("\r\n\r\n",1)[1]
invite=invite.replace("Content-Length: 143","Content-Length: "+str(len(body)))

deadline=time.time()+25
s.sendto(invite.encode(), ("127.0.0.1",5060))
while time.time()<deadline:
    try:
        data,_=s.recvfrom(8192)
        txt=data.decode()
        first=txt.split("\r\n")[0]
        if "100" in first or "180" in first:
            print("GOT:", first)
            continue
        if "200 OK" in first:
            print("=== FULL 200 OK ===")
            print(txt)
            break
    except socket.timeout:
        continue
