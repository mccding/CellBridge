//go:build darwin && cgo

// qdc507-usbcfg-profile-aware reads a QDC507 USB profile and, only when
// --apply is supplied, disables the profile's final UAC flag.  It preserves
// the device's current VID/PID and every other feature flag.  The profile
// mapping is the clean-room representation documented by the project's v8
// evidence: <vid>,<pid>,<diag>,<nmea>,<at_port>,<modem>,<rmnet>,<adb>,<uac>.
//
// It deliberately refuses to guess when the modem returns an unexpected
// profile, refuses to change usbnet, and never touches NAS state.
package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdlib.h>
*/
import "C"

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

const (
	bulkTransferMS = 900
	commandTimeout = 3500 * time.Millisecond
)

var (
	usbcfgLine = regexp.MustCompile(`(?i)\+QCFG:\s*"usbcfg"\s*,\s*([^\r\n]+)`)
	usbnetLine = regexp.MustCompile(`(?i)\+QCFG:\s*"usbnet"\s*,\s*([0-9]+)`)
	knownIDs   = [][2]uint16{{0x2c7c, 0x0125}, {0x2ca3, 0x4006}}
)

type endpointCandidate struct {
	Interface   int    `json:"interface"`
	Class       string `json:"class"`
	Subclass    string `json:"subclass"`
	Protocol    string `json:"protocol"`
	EndpointIn  string `json:"endpointIn"`
	EndpointOut string `json:"endpointOut"`
}

type rawCandidate struct {
	iface    int
	class    byte
	subclass byte
	protocol byte
	in       byte
	out      byte
}

func (c rawCandidate) public() endpointCandidate {
	return endpointCandidate{
		Interface:   c.iface,
		Class:       fmt.Sprintf("0x%02x", c.class),
		Subclass:    fmt.Sprintf("0x%02x", c.subclass),
		Protocol:    fmt.Sprintf("0x%02x", c.protocol),
		EndpointIn:  fmt.Sprintf("0x%02x", c.in),
		EndpointOut: fmt.Sprintf("0x%02x", c.out),
	}
}

type usbcfgProfile struct {
	VID   uint16 `json:"vid"`
	PID   uint16 `json:"pid"`
	Diag  uint8  `json:"diag"`
	NMEA  uint8  `json:"nmea"`
	AT    uint8  `json:"atPort"`
	Modem uint8  `json:"modem"`
	RMNet uint8  `json:"rmnet"`
	ADB   uint8  `json:"adb"`
	UAC   uint8  `json:"uac"`
}

func (p usbcfgProfile) targetUACOff() usbcfgProfile {
	p.UAC = 0
	return p
}

func (p usbcfgProfile) command() string {
	return fmt.Sprintf(`AT+QCFG="usbcfg",0x%04X,0x%04X,%d,%d,%d,%d,%d,%d,%d`,
		p.VID, p.PID, p.Diag, p.NMEA, p.AT, p.Modem, p.RMNet, p.ADB, p.UAC)
}

type commandResult struct {
	Command    string `json:"command"`
	ReturnCode int    `json:"returnCode"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
}

type evidence struct {
	Schema     string             `json:"schema"`
	CapturedAt string             `json:"capturedAt"`
	ReadOnly   bool               `json:"readOnly"`
	Applied    bool               `json:"applied"`
	RebootSent bool               `json:"rebootSent"`
	VID        string             `json:"detectedVid,omitempty"`
	PID        string             `json:"detectedPid,omitempty"`
	Selected   *endpointCandidate `json:"selected,omitempty"`
	USBNet     string             `json:"usbnet,omitempty"`
	Original   *usbcfgProfile     `json:"originalProfile,omitempty"`
	Target     *usbcfgProfile     `json:"targetProfile,omitempty"`
	BackupNote string             `json:"backupNote,omitempty"`
	Commands   []commandResult    `json:"commands,omitempty"`
	Status     string             `json:"status"`
	Error      string             `json:"error,omitempty"`
}

func main() {
	output := flag.String("output", "", "JSON plan/evidence output path")
	apply := flag.Bool("apply", false, "write only the profile-aware UAC=0 target")
	reboot := flag.Bool("reboot", false, "send AT+CFUN=1,1 only after a verified UAC=0 profile")
	flag.Parse()
	if strings.TrimSpace(*output) == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}

	result := run(*apply, *reboot)
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode evidence: %v\n", err)
		os.Exit(2)
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write evidence: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("%s\n", string(mustJSON(map[string]any{
		"status":     result.Status,
		"output":     *output,
		"applied":    result.Applied,
		"rebootSent": result.RebootSent,
	})))
	if result.Status != "PROFILE_READ_ONLY" && result.Status != "UAC_DISABLED_REBOOT_REQUIRED" && result.Status != "UAC_DISABLED_REENUMERATION_REQUIRED" && result.Status != "UAC_ALREADY_OFF" && result.Status != "REBOOT_SENT" {
		os.Exit(2)
	}
}

func run(apply, reboot bool) evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-usbcfg-profile-aware.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   !apply,
		Applied:    false,
		RebootSent: false,
		Status:     "UNKNOWN",
		BackupNote: "originalProfile is the local rollback record; no NAS state is changed",
	}

	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	defer C.libusb_exit(context)

	handle := openKnownDevice(context, &result)
	if handle == nil {
		result.Status = "BLOCKED_QDC507_NOT_FOUND"
		if result.Error == "" {
			result.Error = "QDC507 was not found by libusb under either known identity"
		}
		return result
	}
	defer C.libusb_close(handle)

	candidates, err := discoverCandidates(handle)
	if err != nil {
		result.Status = "BLOCKED_USB_DESCRIPTOR"
		result.Error = err.Error()
		return result
	}
	for _, candidate := range candidates {
		if rc := C.libusb_claim_interface(handle, C.int(candidate.iface)); rc != 0 {
			continue
		}
		response, probeErr := sendCommand(handle, candidate, "AT")
		if probeErr != nil || !looksLikeATSuccess(response) {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			continue
		}
		selected := candidate.public()
		result.Selected = &selected
		result.Commands = append(result.Commands, commandResult{Command: "AT", ReturnCode: 0, Response: trim(response)})

		usbnetResponse, err := sendCommand(handle, candidate, `AT+QCFG="usbnet"`)
		result.Commands = append(result.Commands, commandResult{Command: `AT+QCFG="usbnet"`, ReturnCode: commandCode(err), Response: trim(usbnetResponse), Error: errorText(err)})
		if err != nil {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USB_NET_READ"
			result.Error = err.Error()
			return result
		}
		usbnet, err := parseUSBNet(usbnetResponse)
		if err != nil {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USB_NET_PARSE"
			result.Error = err.Error()
			return result
		}
		result.USBNet = strconv.Itoa(usbnet)
		if usbnet != 1 {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USB_NET_NOT_ECM"
			result.Error = fmt.Sprintf("refusing profile change because usbnet=%d, expected ECM usbnet=1", usbnet)
			return result
		}

		profileResponse, err := sendCommand(handle, candidate, `AT+QCFG="usbcfg"`)
		result.Commands = append(result.Commands, commandResult{Command: `AT+QCFG="usbcfg"`, ReturnCode: commandCode(err), Response: trim(profileResponse), Error: errorText(err)})
		if err != nil {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USBCFG_READ"
			result.Error = err.Error()
			return result
		}
		profile, err := parseProfile(profileResponse)
		if err != nil {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USBCFG_PROFILE"
			result.Error = err.Error()
			return result
		}
		result.Original = &profile
		target := profile.targetUACOff()
		result.Target = &target
		if profile.UAC == 0 {
			if reboot {
				rebootResponse, rebootErr := sendCommand(handle, candidate, "AT+CFUN=1,1")
				result.Commands = append(result.Commands, commandResult{Command: "AT+CFUN=1,1", ReturnCode: commandCode(rebootErr), Response: trim(rebootResponse), Error: errorText(rebootErr)})
				if rebootErr != nil && !errors.Is(rebootErr, errUSBTimeout) {
					C.libusb_release_interface(handle, C.int(candidate.iface))
					result.Status = "BLOCKED_REBOOT"
					result.Error = rebootErr.Error()
					return result
				}
				result.RebootSent = true
				result.Status = "REBOOT_SENT"
				C.libusb_release_interface(handle, C.int(candidate.iface))
				return result
			}
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "UAC_ALREADY_OFF"
			return result
		}
		if !apply {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "PROFILE_READ_ONLY"
			return result
		}

		writeResponse, writeErr := sendCommand(handle, candidate, target.command())
		result.Commands = append(result.Commands, commandResult{Command: target.command(), ReturnCode: commandCode(writeErr), Response: trim(writeResponse), Error: errorText(writeErr)})
		if writeErr != nil || !looksLikeATSuccess(writeResponse) {
			C.libusb_release_interface(handle, C.int(candidate.iface))
			result.Status = "BLOCKED_USBCFG_WRITE"
			result.Error = fmt.Sprintf("profile write did not return clean OK: %v", writeErr)
			return result
		}
		result.Applied = true
		if reboot {
			rebootResponse, rebootErr := sendCommand(handle, candidate, "AT+CFUN=1,1")
			result.Commands = append(result.Commands, commandResult{Command: "AT+CFUN=1,1", ReturnCode: commandCode(rebootErr), Response: trim(rebootResponse), Error: errorText(rebootErr)})
			// The module may close the USB session immediately after accepting CFUN.
			if rebootErr != nil && !errors.Is(rebootErr, errUSBTimeout) {
				C.libusb_release_interface(handle, C.int(candidate.iface))
				result.Status = "BLOCKED_REBOOT"
				result.Error = rebootErr.Error()
				return result
			}
			result.RebootSent = true
		}
		C.libusb_release_interface(handle, C.int(candidate.iface))
		if reboot {
			result.Status = "UAC_DISABLED_REBOOT_REQUIRED"
		} else {
			result.Status = "UAC_DISABLED_REENUMERATION_REQUIRED"
		}
		return result
	}

	result.Status = "BLOCKED_AT_UNAVAILABLE"
	result.Error = "bulk vendor interfaces were found, but none accepted AT"
	return result
}

func openKnownDevice(context *C.libusb_context, result *evidence) *C.libusb_device_handle {
	for _, id := range knownIDs {
		handle := C.libusb_open_device_with_vid_pid(context, C.uint16_t(id[0]), C.uint16_t(id[1]))
		if handle != nil {
			result.VID = fmt.Sprintf("0x%04x", id[0])
			result.PID = fmt.Sprintf("0x%04x", id[1])
			return handle
		}
	}
	return nil
}

func parseUSBNet(response string) (int, error) {
	matches := usbnetLine.FindStringSubmatch(response)
	if len(matches) != 2 {
		return 0, fmt.Errorf("usbnet response has no parseable +QCFG line: %q", trim(response))
	}
	value, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, fmt.Errorf("parse usbnet=%q: %w", matches[1], err)
	}
	return value, nil
}

func parseProfile(response string) (usbcfgProfile, error) {
	matches := usbcfgLine.FindStringSubmatch(response)
	if len(matches) != 2 {
		return usbcfgProfile{}, fmt.Errorf("usbcfg response has no parseable +QCFG line: %q", trim(response))
	}
	parts := strings.Split(matches[1], ",")
	if len(parts) != 9 {
		return usbcfgProfile{}, fmt.Errorf("refusing unknown usbcfg shape: got %d fields, expected 9", len(parts))
	}
	values := make([]uint64, len(parts))
	for i, part := range parts {
		value, err := strconv.ParseUint(strings.TrimSpace(part), 0, 32)
		if err != nil {
			return usbcfgProfile{}, fmt.Errorf("parse usbcfg field %d=%q: %w", i, part, err)
		}
		values[i] = value
	}
	for i, value := range values[2:] {
		if value > 1 {
			return usbcfgProfile{}, fmt.Errorf("refusing usbcfg feature field %d=%d; expected binary 0/1", i+2, value)
		}
	}
	return usbcfgProfile{
		VID: uint16(values[0]), PID: uint16(values[1]),
		Diag: uint8(values[2]), NMEA: uint8(values[3]), AT: uint8(values[4]),
		Modem: uint8(values[5]), RMNet: uint8(values[6]), ADB: uint8(values[7]), UAC: uint8(values[8]),
	}, nil
}

func discoverCandidates(handle *C.libusb_device_handle) ([]rawCandidate, error) {
	device := C.libusb_get_device(handle)
	if device == nil {
		return nil, errors.New("libusb returned no device for the opened handle")
	}
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(device, &config); rc != 0 {
		return nil, fmt.Errorf("get active configuration: %s", usbError(rc))
	}
	defer C.libusb_free_config_descriptor(config)
	var candidates []rawCandidate
	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		altSettings := unsafe.Slice(intf.altsetting, int(intf.num_altsetting))
		for _, alt := range altSettings {
			var in, out byte
			endpoints := unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints))
			for _, endpoint := range endpoints {
				if byte(endpoint.bmAttributes)&byte(C.LIBUSB_TRANSFER_TYPE_MASK) != byte(C.LIBUSB_TRANSFER_TYPE_BULK) {
					continue
				}
				address := byte(endpoint.bEndpointAddress)
				if address&byte(C.LIBUSB_ENDPOINT_IN) != 0 {
					in = address
				} else {
					out = address
				}
			}
			if in != 0 && out != 0 {
				candidates = append(candidates, rawCandidate{iface: int(alt.bInterfaceNumber), class: byte(alt.bInterfaceClass), subclass: byte(alt.bInterfaceSubClass), protocol: byte(alt.bInterfaceProtocol), in: in, out: out})
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidatePriority(candidates[i].iface) < candidatePriority(candidates[j].iface)
	})
	return candidates, nil
}

func candidatePriority(iface int) int {
	if iface == 2 || iface == 3 {
		return 0
	}
	return 1
}

func sendCommand(handle *C.libusb_device_handle, candidate rawCandidate, command string) (string, error) {
	if _, err := drain(handle, candidate.in); err != nil {
		return "", err
	}
	payload := append([]byte(strings.TrimSpace(command)), '\r')
	if err := bulkWrite(handle, candidate.out, payload); err != nil {
		return "", fmt.Errorf("write %s: %w", command, err)
	}
	deadline := time.Now().Add(commandTimeout)
	var response strings.Builder
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining > 900*time.Millisecond {
			remaining = 900 * time.Millisecond
		}
		chunk, err := bulkRead(handle, candidate.in, int(remaining.Milliseconds()))
		if err != nil {
			if errors.Is(err, errUSBTimeout) {
				continue
			}
			return response.String(), err
		}
		response.Write(chunk)
		if responseComplete(response.String()) {
			return response.String(), nil
		}
	}
	if response.Len() == 0 {
		return "", errUSBTimeout
	}
	return response.String(), nil
}

var errUSBTimeout = errors.New("USB transfer timed out")

func drain(handle *C.libusb_device_handle, endpoint byte) ([]byte, error) {
	var all []byte
	for i := 0; i < 4; i++ {
		chunk, err := bulkRead(handle, endpoint, 80)
		if errors.Is(err, errUSBTimeout) {
			return all, nil
		}
		if err != nil {
			return all, err
		}
		all = append(all, chunk...)
	}
	return all, nil
}

func bulkWrite(handle *C.libusb_device_handle, endpoint byte, payload []byte) error {
	buffer := C.CBytes(payload)
	defer C.free(buffer)
	transferred := C.int(0)
	rc := C.libusb_bulk_transfer(handle, C.uchar(endpoint), (*C.uchar)(buffer), C.int(len(payload)), &transferred, C.uint(bulkTransferMS))
	if rc != 0 {
		return fmt.Errorf("bulk write: %s", usbError(rc))
	}
	if int(transferred) != len(payload) {
		return fmt.Errorf("bulk write short transfer: %d/%d bytes", transferred, len(payload))
	}
	return nil
}

func bulkRead(handle *C.libusb_device_handle, endpoint byte, timeoutMS int) ([]byte, error) {
	if timeoutMS < 1 {
		timeoutMS = 1
	}
	buffer := C.malloc(4096)
	defer C.free(buffer)
	transferred := C.int(0)
	rc := C.libusb_bulk_transfer(handle, C.uchar(endpoint), (*C.uchar)(buffer), 4096, &transferred, C.uint(timeoutMS))
	if rc == C.LIBUSB_ERROR_TIMEOUT {
		return nil, errUSBTimeout
	}
	if rc != 0 {
		return nil, fmt.Errorf("bulk read: %s", usbError(rc))
	}
	return C.GoBytes(buffer, transferred), nil
}

func responseComplete(response string) bool {
	upper := strings.ToUpper(response)
	return strings.Contains(upper, "\nOK") || strings.Contains(upper, "\nERROR") || strings.HasSuffix(strings.TrimSpace(upper), "OK") || strings.HasSuffix(strings.TrimSpace(upper), "ERROR")
}

func looksLikeATSuccess(response string) bool {
	upper := strings.ToUpper(response)
	return strings.Contains(upper, "OK") && !strings.Contains(upper, "ERROR")
}

func trim(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) > 16384 {
		return value[:16384] + "\n...[truncated]"
	}
	return value
}

func commandCode(err error) int {
	if err != nil {
		return -1
	}
	return 0
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func usbError(code C.int) string {
	name := C.libusb_error_name(code)
	if name == nil {
		return fmt.Sprintf("libusb error %d", int(code))
	}
	return C.GoString(name)
}

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}
