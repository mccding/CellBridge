package sip

import (
 "bytes"
 "context"
 "encoding/json"
 "log/slog"
 "net/http"
 "strings"
 "time"
)

// sendYakPush fires the official YakPhone PushKit notification
// (push.yakteam.com/v1/notify) so the phone wakes for an incoming SIP
// call / missed call / SIP message even when YakPhone is suspended.
func (s *Server) sendYakPush(callerURI, pushType, messageBody string) {
 if s.pushToken == "" {
  slog.Warn("yakpush skipped", "reason", "no push token configured", "type", pushType)
  return
 }
 body := map[string]string{
  "token": s.pushToken,
  "caller_uri": callerURI,
  "caller_name": strings.TrimPrefix(callerURI, "sip:"),
  "type": pushType,
 }
 if messageBody != "" { body["message_body"] = messageBody }
 payload, err := json.Marshal(body)
 if err != nil { return }
 req, err := http.NewRequest(http.MethodPost, "https://push.yakteam.com/v1/notify", bytes.NewReader(payload))
 if err != nil { return }
 req.Header.Set("Content-Type", "application/json")
 client := &http.Client{Timeout: 10 * time.Second}
 resp, err := client.Do(req)
 if err != nil {
  slog.Warn("yakpush failed", "type", pushType, "err", err)
  return
 }
 defer resp.Body.Close()
 slog.Info("yakpush sent", "type", pushType, "status", resp.Status)
}

var _ = context.Background
