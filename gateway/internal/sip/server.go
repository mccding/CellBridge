package sip

import (
 "context"
 "fmt"
 "log/slog"
 "net"
 "strings"
 "sync"
 "time"

 "github.com/cellbridge/cellbridge/gateway/internal/modem"
 "github.com/google/uuid"
)

type Server struct {
 listenAddr string
 conn *net.UDPConn
 registrar *Registrar
 auth *Auth
 modem *modem.ActiveCallAdapter
 audio modem.VoiceAudio
 events        <-chan modem.ModemEvent
 	sessions      sync.Map
 	pushToken     string
 	sendSMS       func(ctx context.Context, to, body string) error
 	ctx           context.Context
 	cancel        context.CancelFunc
}

func NewServer(listenAddr string, registrar *Registrar, auth *Auth, modemCtl *modem.ActiveCallAdapter, audio modem.VoiceAudio) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{listenAddr: listenAddr, registrar: registrar, auth: auth, modem: modemCtl, audio: audio, ctx: ctx, cancel: cancel}
}

// AttachEvents wires modem events (RING etc) and the YakPhone push token
// from config so inbound cellular calls can ring the SIP client (§21).
func (s *Server) AttachEvents(events <-chan modem.ModemEvent, pushToken string) {
	s.events = events
	s.pushToken = pushToken
}

// AttachSMS wires the SMS engine so SIP MESSAGE requests from the phone
// are delivered over the cellular modem (final architecture: SMS also
// routes through the NAS Gateway).
func (s *Server) AttachSMS(send func(ctx context.Context, to, body string) error) {
	s.sendSMS = send
}

func (s *Server) Start(ctx context.Context) error {
 addr, err := net.ResolveUDPAddr("udp", s.listenAddr)
 if err != nil { return err }
 conn, err := net.ListenUDP("udp", addr)
 if err != nil { return err }
 s.conn = conn
 slog.Info("sip server listening", "addr", s.listenAddr)
 go s.readLoop()
 go s.inboundLoop()
 return nil
}

func (s *Server) Stop(ctx context.Context) error {
 s.cancel()
 if s.conn != nil { _ = s.conn.Close() }
 s.sessions.Range(func(k, v interface{}) bool { _ = v.(*SIPCallSession).Hangup(); return true })
 return nil
}

func (s *Server) AddUser(username, password string) { s.auth.AddUser(username, password) }

func (s *Server) readLoop() {
 buf := make([]byte, 8192)
 for {
  n, remote, err := s.conn.ReadFromUDP(buf)
  if err != nil {
   select { case <-s.ctx.Done(): return; default: }
   continue
  }
  msg := string(buf[:n])
  go s.handleMessage(msg, remote)
 }
}

// inboundLoop watches modem events. On an incoming cellular call (RING /
// +CLIP), it sends a SIP INVITE to every registered client (§21) and fires
// the YakPhone PushKit notification so the phone wakes even when the app
// is suspended.
func (s *Server) inboundLoop() {
 for {
  select {
  case <-s.ctx.Done():
   return
  case event, ok := <-s.events:
   if !ok { return }
   if event.Kind != "incoming" { continue }
   go s.ringClients(event)
  }
 }
}

func (s *Server) ringClients(event modem.ModemEvent) {
 peer := event.Peer
 if peer == "" { peer = "unknown" }
 callID := "in-" + uuid.NewString()[:12]
 media, err := NewMediaSession("0.0.0.0:0")
 if err != nil { return }
 sess := NewSIPCallSession(callID, peer, "inbound", s.modem, s.audio, media)
 s.sessions.Store(callID, sess)
 sdp := fmt.Sprintf("v=0\r\no=cellbridge 0 0 IN IP4 %s\r\ns=CellBridge\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", s.nasIP(), s.nasIP(), media.LocalAddr().Port)
 for _, reg := range s.registrar.All() {
  remote := contactAddr(reg.Contact)
  if remote == nil { continue }
  invite := fmt.Sprintf("INVITE sip:%s@%s SIP/2.0\r\nVia: SIP/2.0/UDP %s:5060;branch=z9hG4bK%s;rport\r\nFrom: <sip:%s@%s>;tag=cb%s\r\nTo: <sip:%s@%s>\r\nCall-ID: %s\r\nCSeq: 1 INVITE\r\nContact: <sip:cellbridge@%s>\r\nContent-Type: application/sdp\r\nContent-Length: %d\r\n\r\n%s", reg.Username, s.nasIP(), s.nasIP(), callID[:8], peer, s.nasIP(), callID[:8], reg.Username, s.nasIP(), callID, s.nasIP(), len(sdp), sdp)
  if _, err := s.conn.WriteToUDP([]byte(invite), remote); err != nil {
   slog.Warn("sip inbound invite failed", "user", reg.Username, "err", err)
  }
 }
 s.sendYakPush("sip:"+peer+"@"+s.nasIP(), "voip", "")
 slog.Info("sip inbound ringing", "call_id", callID, "peer", peer)
}

// contactAddr extracts host:port from a SIP Contact header value.
func contactAddr(contact string) *net.UDPAddr {
 if i := strings.Index(contact, "sip:"); i >= 0 {
  rest := contact[i+4:]
  if j := strings.IndexAny(rest, ">;"); j >= 0 { rest = rest[:j] }
  if addr, err := net.ResolveUDPAddr("udp", rest); err == nil { return addr }
 }
 return nil
}

func (s *Server) handleMessage(msg string, remote *net.UDPAddr) {
 lines := strings.Split(msg, "\r\n")
 if len(lines)==0 { return }
 first := lines[0]
 if strings.HasPrefix(first, "REGISTER") { s.handleRegister(msg, remote); return }
 if strings.HasPrefix(first, "INVITE") { s.handleInvite(msg, remote); return }
 if strings.HasPrefix(first, "ACK") {
  // ACK is part of the existing INVITE transaction; it is never answered.
  return
 }
 if strings.HasPrefix(first, "BYE") || strings.HasPrefix(first, "CANCEL") { s.handleAckBye(msg, remote, first); return }
 if strings.HasPrefix(first, "OPTIONS") { s.sendResponse(remote, msg, 200, "OK", "", ""); return }
	if strings.HasPrefix(first, "MESSAGE") { s.handleMessageRequest(msg, remote); return }
}

func parseHeader(msg, name string) string {
	for _, line := range strings.Split(msg, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), strings.ToLower(name)+":") { return strings.TrimSpace(line[len(name)+1:]) }
	}
	return ""
}

// handleMessageRequest delivers a SIP MESSAGE (from YakPhone, which is
// baresip-based and sends SMS as SIP instant messages) over the cellular
// modem via the attached SMS sender (§30). Responds 200 on acceptance,
// 500 when no SMS engine is wired or the modem rejects the submission.
func (s *Server) handleMessageRequest(msg string, remote *net.UDPAddr) {
	if s.sendSMS == nil {
		slog.Warn("sip message rejected", "reason", "SMS engine not attached")
		s.sendResponse(remote, msg, 500, "Server Error", "", "")
		return
	}
	// The destination is the Request-URI user part: MESSAGE sip:185xxx@host.
	requestURI := strings.Fields(msg)[1]
	destination := requestURI
	if index := strings.Index(requestURI, "@"); index > 0 {
		destination = strings.TrimPrefix(requestURI[:index], "sip:")
	}
	destination = strings.TrimLeft(destination, "+\x20")
	if destination == "" {
		slog.Warn("sip message rejected", "reason", "empty destination")
		s.sendResponse(remote, msg, 400, "Bad Request", "", "")
		return
	}
	// Message body follows the blank line (Content-Type: text/plain).
	body := ""
	if sections := strings.SplitN(msg, "\r\n\r\n", 2); len(sections) == 2 {
		body = strings.TrimSpace(sections[1])
	}
	if body == "" {
		slog.Warn("sip message rejected", "reason", "empty body", "to", destination)
		s.sendResponse(remote, msg, 400, "Bad Request", "", "")
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.sendSMS(ctx, destination, body); err != nil {
		slog.Warn("sip message send failed", "to", destination, "err", err)
		s.sendResponse(remote, msg, 500, "Server Error", "", "")
		return
	}
	slog.Info("sip message sent", "to", destination, "length", len(body))
	s.sendResponse(remote, msg, 200, "OK", "", "")
}

func (s *Server) handleRegister(msg string, remote *net.UDPAddr) {
 from := parseHeader(msg, "From")
 to := parseHeader(msg, "To")
 contact := parseHeader(msg, "Contact")
 expiresStr := parseHeader(msg, "Expires")
 username := extractSIPUser(to)
 if username == "" { username = extractSIPUser(from) }
 if s.auth.NeedsAuth(msg) {
  hdrs := "WWW-Authenticate: " + WWWAuthHeader(s.auth.Realm(), s.auth.Nonce()) + "\r\n"
  s.sendResponse(remote, msg, 401, "Unauthorized", hdrs, "")
  return
 }
 expires := 3600
 if expiresStr != "" { fmt.Sscanf(expiresStr, "%d", &expires) }
 if strings.Contains(contact, "expires=0") { expires = 0 }
 s.registrar.Register(username, contact, "UDP", expires)
 s.sendResponse(remote, msg, 200, "OK", "Contact: "+contact+"\r\nExpires: "+fmt.Sprintf("%d", expires)+"\r\n", "")
 slog.Info("sip register", "user", username, "contact", contact, "expires", expires)
}

// handleInvite implements §20: invite -> 100 -> modem dial -> 180 -> wait
// cellular answer (PCM RUNNING) -> 200 OK. Retransmissions of the same
// Call-ID answer with current state, never a second dial.
func (s *Server) handleInvite(msg string, remote *net.UDPAddr) {
 from := parseHeader(msg, "From")
 to := parseHeader(msg, "To")
 peer := extractSIPUser(to)
 if peer == "" { peer = "unknown" }
 username := extractSIPUser(from)
 if _, ok := s.registrar.Get(username); !ok { s.sendResponse(remote, msg, 403, "Forbidden", "", ""); return }
 callID := parseHeader(msg, "Call-ID")
 if callID == "" { callID = uuid.NewString() }
 if sess, ok := s.sessions.Load(callID); ok {
  existing := sess.(*SIPCallSession)
  hdrs := "Contact: <sip:cellbridge@" + s.nasIP() + ">\r\nAllow: INVITE, ACK, BYE, CANCEL, OPTIONS\r\nContent-Type: application/sdp\r\n"
  if rx, tx := existing.media.Stats(); rx > 0 || tx > 0 || existing.State() == "active" {
   sdp := fmt.Sprintf("v=0\r\no=cellbridge 0 0 IN IP4 %s\r\ns=CellBridge\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", s.nasIP(), s.nasIP(), existing.media.LocalAddr().Port)
   s.sendResponse(remote, msg, 200, "OK", hdrs, sdp)
  } else {
   s.sendResponse(remote, msg, 180, "Ringing", "Contact: <sip:cellbridge@"+s.nasIP()+">\r\n", "")
  }
  return
 }
 media, err := NewMediaSession("0.0.0.0:0")
 if err != nil {
  s.sendResponse(remote, msg, 500, "Server Error", "", "")
  return
 }
 clientSDP := extractSDP(msg)
 rtpIP, rtpPort := parseSDPRTP(clientSDP)
 // YakPhone/baresip may advertise its public WAN address in the SDP c= line;
 // inside the tailnet that address is unreachable. Always use the INVITE
 // packet's source IP with the SDP media port.
 if rtpPort != 0 { _ = media.SetRemote(fmt.Sprintf("%s:%d", remote.IP.String(), rtpPort)) }

 s.sendResponse(remote, msg, 100, "Trying", "", "")
 sess := NewSIPCallSession(callID, peer, "outbound", s.modem, s.audio, media)
 if err := sess.Dial(); err != nil {
  slog.Warn("sip invite dial failed", "err", err)
  s.sendResponse(remote, msg, 500, "Server Error", "", "")
  s.sessions.Delete(callID)
  _ = media.Close()
  return
 }
 s.sessions.Store(callID, sess)
 s.sendResponse(remote, msg, 180, "Ringing", "Contact: <sip:cellbridge@"+s.nasIP()+">\r\n", "")
 go func() {
  answerCtx, cancel := context.WithTimeout(context.Background(), answerTimeout)
  defer cancel()
  if err := sess.AwaitBridge(answerCtx); err != nil {
   slog.Warn("sip await bridge failed", "call", callID, "err", err)
   _ = sess.Hangup()
   s.sessions.Delete(callID)
   return
  }
  sdp := fmt.Sprintf("v=0\r\no=cellbridge 0 0 IN IP4 %s\r\ns=CellBridge\r\nc=IN IP4 %s\r\nt=0 0\r\nm=audio %d RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n", s.nasIP(), s.nasIP(), media.LocalAddr().Port)
  hdrs := "Contact: <sip:cellbridge@" + s.nasIP() + ">\r\nAllow: INVITE, ACK, BYE, CANCEL, OPTIONS\r\nContent-Type: application/sdp\r\n"
  s.sendResponse(remote, msg, 200, "OK", hdrs, sdp)
  slog.Info("sip invite handled", "call", callID, "peer", peer, "rtp_remote", fmt.Sprintf("%s:%d", rtpIP, rtpPort))
 }()
}

func (s *Server) handleAckBye(msg string, remote *net.UDPAddr, first string) {
 callID := parseHeader(msg, "Call-ID")
 if callID == "" { s.sendResponse(remote, msg, 200, "OK", "", ""); return }
 if v, ok := s.sessions.Load(callID); ok {
  sess := v.(*SIPCallSession)
  if strings.HasPrefix(first, "BYE") || strings.HasPrefix(first, "CANCEL") {
   slog.Info("sip bye received", "call", callID, "method", strings.Fields(first)[0])
   _ = sess.Hangup()
   s.sessions.Delete(callID)
  }
 }
 s.sendResponse(remote, msg, 200, "OK", "", "")
}

func (s *Server) nasIP() string {
 if ip := localTailnetIP(); ip != "" { return ip }
 return "127.0.0.1"
}

func (s *Server) sendResponse(remote *net.UDPAddr, req string, code int, reason, extraHeaders, body string) {
 callID := parseHeader(req, "Call-ID")
 from := parseHeader(req, "From")
 to := parseHeader(req, "To")
 via := parseHeader(req, "Via")
 cseq := parseHeader(req, "CSeq")
 tag := ";tag=" + uuid.NewString()[:8]
 if strings.Contains(to, "tag=") { tag = "" }
 resp := fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s%s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: %d\r\n\r\n%s", code, reason, via, from, to, tag, callID, cseq, extraHeaders, len(body), body)
 _, _ = s.conn.WriteToUDP([]byte(resp), remote)
}

func extractSIPUser(hdr string) string {
 s := hdr
 if i := strings.Index(s, "sip:"); i >= 0 { s = s[i+4:] } else { return "" }
 if j := strings.Index(s, "@"); j >= 0 { return s[:j] }
 if j := strings.Index(s, ">"); j >= 0 { return s[:j] }
 return strings.Fields(s)[0]
}
func extractSDP(msg string) string { parts := strings.Split(msg, "\r\n\r\n"); if len(parts) < 2 { return "" }; return parts[1] }
func parseSDPRTP(sdp string) (string, int) {
 var ip string; var port int
 for _, line := range strings.Split(sdp, "\n") {
  line = strings.TrimSpace(line)
  if strings.HasPrefix(line, "c=IN IP4 ") { ip = strings.TrimSpace(line[len("c=IN IP4 "):]) }
  if strings.HasPrefix(line, "m=audio ") { fmt.Sscanf(line, "m=audio %d", &port) }
 }
 return ip, port
}

// answerTimeout bounds how long we wait for the cellular leg to be
// answered before giving up on the SIP call.
const answerTimeout = 90 * time.Second

// localTailnetIP returns the NAS tailnet IPv4 address by walking its
// interfaces, preferring tailscale0.
func localTailnetIP() string {
 if addrs, err := net.InterfaceAddrs(); err == nil {
  for _, a := range addrs {
   if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
    if strings.HasPrefix(ipnet.IP.String(), "100.") { return ipnet.IP.String() }
   }
  }
 }
 return ""
}
