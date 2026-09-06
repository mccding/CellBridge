import socket, time

s=socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.settimeout(2)
time.sleep(0.5)
bye=("BYE sip:10010@YOURNAS.ts.net SIP/2.0\r\n"
  "Via: SIP/2.0/UDP 127.0.0.1:5090;branch=z9hG4bKselfbye2;rport\r\n"
  "From: <sip:1001@YOURNAS.ts.net>;tag=selfinv2\r\n"
  "To: <sip:10010@YOURNAS.ts.net>;\r\n"
  "Call-ID: selfinv2@127.0.0.1\r\n"
  "CSeq: 2 BYE\r\n"
  "Content-Length: 0\r\n\r\n")
s.sendto(bye.encode(), ("127.0.0.1", 5060))
try:
    data,_=s.recvfrom(8192)
    print("BYE:", data.decode().split("\r\n")[0])
except Exception as e:
    print("TIMEOUT", e)
