package sip

import (
 "sync"
 "time"
)

type Registration struct {
 Username string
 Contact string
 Expires time.Time
 Transport string
 LastSeen time.Time
}

type Registrar struct {
 mu sync.Mutex
 regs map[string]*Registration
}

func NewRegistrar() *Registrar { return &Registrar{regs: make(map[string]*Registration)} }

func (r *Registrar) Register(username, contact, transport string, expiresSec int) {
 r.mu.Lock()
 defer r.mu.Unlock()
 if expiresSec == 0 {
  delete(r.regs, username)
  return
 }
 r.regs[username] = &Registration{
  Username: username,
  Contact: contact,
  Transport: transport,
  LastSeen: time.Now(),
  Expires: time.Now().Add(time.Duration(expiresSec) * time.Second),
 }
}

func (r *Registrar) Get(username string) (*Registration, bool) {
 r.mu.Lock()
 defer r.mu.Unlock()
 reg, ok := r.regs[username]
 if !ok { return nil, false }
 if time.Now().After(reg.Expires) {
  delete(r.regs, username)
  return nil, false
 }
 return reg, true
}

func (r *Registrar) All() []*Registration {
 r.mu.Lock()
 defer r.mu.Unlock()
 var out []*Registration
 for _, v := range r.regs {
  if time.Now().Before(v.Expires) { out = append(out, v) }
 }
 return out
}
