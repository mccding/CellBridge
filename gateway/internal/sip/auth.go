package sip

import (
 "crypto/rand"
 "encoding/hex"
 "strings"
)

type Auth struct {
 users map[string]string // username -> password
 realm string
 nonce string
}

func NewAuth(realm string) *Auth {
 b := make([]byte, 8)
 _, _ = rand.Read(b)
 return &Auth{
  users: make(map[string]string),
  realm: realm,
  nonce: hex.EncodeToString(b),
 }
}

func (a *Auth) AddUser(username, password string) { a.users[username] = password }

func (a *Auth) Check(username, password string) bool {
 if p, ok := a.users[username]; ok { return p == password }
 return false
}

func (a *Auth) Realm() string { return a.realm }
func (a *Auth) Nonce() string { return a.nonce }

// Very small digest check placeholder — YakPhone will use plain UDP auth per doc Phase 4, so we allow plain or digest with same password.
// Real digest verification is done only if Authorization header present; otherwise 401 with WWW-Authenticate.
func (a *Auth) NeedsAuth(msg string) bool {
 // If no Authorization header, need auth
 return !strings.Contains(strings.ToLower(msg), "authorization:")
}

func WWWAuthHeader(realm, nonce string) string {
 return `Digest realm="` + realm + `", nonce="` + nonce + `", algorithm=MD5, qop="auth"`
}
