package mdns

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	group       = "224.0.0.251:5353"
	serviceType = "_cellbridge-module._tcp.local."
)

type Advertiser struct {
	Name         string
	Host         string
	IP           net.IP
	Port         uint16
	DeviceID     string
	AgentVersion string
}

func (a Advertiser) Run(stop <-chan struct{}) error {
	if a.Name == "" || a.Host == "" || a.IP.To4() == nil || a.Port == 0 {
		return fmt.Errorf("invalid mDNS advertisement configuration")
	}
	// Bind the source socket to the same explicit ECM address as the HTTP
	// listener. A wildcard source could make the advertisement leave through a
	// cellular/WAN route, which v8 explicitly forbids.
	connection, err := net.ListenUDP("udp4", &net.UDPAddr{IP: a.IP.To4(), Port: 0})
	if err != nil {
		return err
	}
	defer connection.Close()
	destination, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return err
	}
	send := func() error {
		_, err := connection.WriteToUDP(packet(a), destination)
		return err
	}
	if err := send(); err != nil {
		return err
	}
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			if err := send(); err != nil {
				return err
			}
		}
	}
}

func packet(a Advertiser) []byte {
	service := strings.TrimSuffix(a.Name, ".") + "._cellbridge-module._tcp.local."
	host := strings.TrimSuffix(a.Host, ".") + ".local."
	ptr := serviceType
	txt := []string{
		"version=1",
		"device_family=qdc507",
		"agent_version=" + a.AgentVersion,
		"device_id=" + a.DeviceID,
	}
	var body []byte
	body = append(body, rr(ptr, 12, 120, encodeName(service))...)
	body = append(body, rr(service, 33, 120, srv(0, 0, a.Port, host))...)
	body = append(body, rr(service, 16, 120, txtRecord(txt))...)
	body = append(body, rr(host, 1, 120, a.IP.To4())...)
	header := make([]byte, 12)
	binary.BigEndian.PutUint16(header[0:2], 0)
	binary.BigEndian.PutUint16(header[2:4], 0x8400)
	binary.BigEndian.PutUint16(header[4:6], 0)
	binary.BigEndian.PutUint16(header[6:8], 4)
	binary.BigEndian.PutUint16(header[8:10], 4)
	binary.BigEndian.PutUint16(header[10:12], 0)
	return append(header, body...)
}

func rr(name string, kind, ttl uint32, data []byte) []byte {
	encoded := encodeName(name)
	header := make([]byte, 10)
	binary.BigEndian.PutUint16(header[0:2], uint16(kind))
	binary.BigEndian.PutUint16(header[2:4], 1)
	binary.BigEndian.PutUint32(header[4:8], ttl)
	binary.BigEndian.PutUint16(header[8:10], uint16(len(data)))
	return append(append(encoded, header...), data...)
}

func encodeName(name string) []byte {
	var result []byte
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if len(label) > 63 {
			label = label[:63]
		}
		result = append(result, byte(len(label)))
		result = append(result, label...)
	}
	return append(result, 0)
}

func srv(priority, weight, port uint16, target string) []byte {
	result := make([]byte, 6)
	binary.BigEndian.PutUint16(result[0:2], priority)
	binary.BigEndian.PutUint16(result[2:4], weight)
	binary.BigEndian.PutUint16(result[4:6], port)
	return append(result, encodeName(target)...)
}

func txtRecord(records []string) []byte {
	var result []byte
	for _, record := range records {
		if len(record) > 255 {
			record = record[:255]
		}
		result = append(result, byte(len(record)))
		result = append(result, record...)
	}
	return result
}
