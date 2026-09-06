package discovery

import (
	"fmt"
	"net"
	"strings"

	"github.com/grandcat/zeroconf"
)

// Advertiser owns only the public discovery record. Credentials, phone
// numbers, IMSI values, and modem secrets never enter TXT records.
type Advertiser struct {
	server *zeroconf.Server
}

func RegisterPocket(instance, gatewayID, transport, listenAddress string) (*Advertiser, error) {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		instance = "CellBridge PocketBridge"
	}
	gatewayID = strings.TrimSpace(gatewayID)
	if gatewayID == "" {
		return nil, fmt.Errorf("gateway id is required")
	}
	transport = strings.TrimSpace(transport)
	if transport != "pocketUSB" && transport != "pocketWiFi" {
		return nil, fmt.Errorf("pocket transport must be pocketUSB or pocketWiFi")
	}
	_, portText, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return nil, fmt.Errorf("parse listen address: %w", err)
	}
	var port int
	if _, err := fmt.Sscanf(portText, "%d", &port); err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid listen port %q", portText)
	}
	server, err := zeroconf.Register(
		instance,
		"_cellbridge._tcp",
		"local.",
		port,
		[]string{
			"version=1",
			"api_version=v1",
			"gateway_id=" + gatewayID,
			"mode=pocket",
			"transport=" + transport,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("register _cellbridge._tcp: %w", err)
	}
	return &Advertiser{server: server}, nil
}

func (a *Advertiser) Shutdown() {
	if a != nil && a.server != nil {
		a.server.Shutdown()
	}
}
