package mdns

import (
	"net"
	"strings"
	"testing"
)

func TestPacketContainsOnlyApprovedTXT(t *testing.T) {
	packet := packet(Advertiser{Name: "CellBridge QDC507", Host: "cellbridge-qdc507", IP: net.ParseIP("192.0.2.2"), Port: 8788, DeviceID: "opaque", AgentVersion: "0.1.0"})
	text := string(packet)
	for _, forbidden := range []string{"IMEI", "IMSI", "ICCID", "token", "secret", "phone"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("forbidden TXT material %q found", forbidden)
		}
	}
}
