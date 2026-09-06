//go:build darwin && cgo

// qdc507-at-readonly opens the known QDC507 USB identity through libusb and
// performs only bounded, read-only AT queries. It deliberately does not
// change USBCFG/usbnet, enable ADB, reboot the module, or install anything.
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
	"sort"
	"strings"
	"time"
	"unsafe"
)

const (
	qdc507VendorID  = 0x2c7c
	qdc507ProductID = 0x0125
	bulkTransferMS  = 900
	commandTimeout  = 3500 * time.Millisecond
)

var readonlyCommands = []string{
	"AT",
	"ATI",
	`AT+QCFG="usbnet"`,
	`AT+QCFG="usbcfg"`,
	"AT+CPIN?",
	"AT+COPS?",
	"AT+CSQ",
	"AT+CLCC",
	"AT+CEER",
	`AT+QCFG="call_control"`,
	"AT+CEREG?",
	"AT+CGREG?",
	"AT+CGATT?",
	"AT+CMGF?",
	"AT+CSCA?",
	"AT+CSMS?",
	"AT+CGSMS?",
	"AT+CMGL=4",
	"AT+CPMS?",
	"AT+CSCS?",
	"AT+CNMI?",
	"AT+VTS=?",
	"AT+QPCMV?",
	"AT+QDAI?",
	"AT+QPCMV=?",
	"AT+QAUDRD=?",
	"AT+QAUDPLAY=?",
	"AT+QAUDSTOP=?",
}

type endpointCandidate struct {
	Interface   int    `json:"interface"`
	Class       string `json:"class"`
	Subclass    string `json:"subclass"`
	Protocol    string `json:"protocol"`
	EndpointIn  string `json:"endpointIn"`
	EndpointOut string `json:"endpointOut"`
}

type commandResult struct {
	Command    string `json:"command"`
	ReturnCode int    `json:"returnCode"`
	Response   string `json:"response,omitempty"`
	Error      string `json:"error,omitempty"`
}

type evidence struct {
	Schema     string              `json:"schema"`
	CapturedAt string              `json:"capturedAt"`
	ReadOnly   bool                `json:"readOnly"`
	VendorID   string              `json:"vendorId"`
	ProductID  string              `json:"productId"`
	Status     string              `json:"status"`
	Candidates []endpointCandidate `json:"candidates,omitempty"`
	Selected   *endpointCandidate  `json:"selected,omitempty"`
	Commands   []commandResult     `json:"commands,omitempty"`
	Error      string              `json:"error,omitempty"`
}

func main() {
	output := flag.String("output", "", "JSON evidence output path")
	flag.Parse()
	if strings.TrimSpace(*output) == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}

	result := runReadOnly()
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode evidence: %v\n", err)
		os.Exit(2)
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write evidence: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("%s\n", string(mustJSON(map[string]string{
		"status": result.Status,
		"output": *output,
	})))
	if result.Status != "READ_ONLY_AT_CAPTURED" {
		os.Exit(2)
	}
}

func runReadOnly() evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-at-readonly.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   true,
		VendorID:   fmt.Sprintf("0x%04x", qdc507VendorID),
		ProductID:  fmt.Sprintf("0x%04x", qdc507ProductID),
		Status:     "UNKNOWN",
	}

	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	defer C.libusb_exit(context)

	handle := C.libusb_open_device_with_vid_pid(context, C.uint16_t(qdc507VendorID), C.uint16_t(qdc507ProductID))
	if handle == nil {
		result.Status = "BLOCKED_QDC507_NOT_FOUND"
		result.Error = "QDC507 USB device 2c7c:0125 was not found by libusb"
		return result
	}
	defer C.libusb_close(handle)

	candidates, err := discoverCandidates(handle)
	if err != nil {
		result.Status = "BLOCKED_USB_DESCRIPTOR"
		result.Error = err.Error()
		return result
	}
	result.Candidates = make([]endpointCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		result.Candidates = append(result.Candidates, candidate.public())
	}
	if len(candidates) == 0 {
		result.Status = "BLOCKED_NO_BULK_VENDOR_INTERFACE"
		result.Error = "no bulk IN/OUT vendor interface was found"
		return result
	}

	for _, candidate := range candidates {
		if rc := C.libusb_claim_interface(handle, C.int(candidate.iface)); rc != 0 {
			result.Commands = append(result.Commands, commandResult{
				Command:    "AT",
				ReturnCode: int(rc),
				Error:      fmt.Sprintf("interface %d: %s", candidate.iface, usbError(rc)),
			})
			continue
		}

		response, probeErr := sendCommand(handle, candidate, "AT")
		if probeErr != nil || !looksLikeATSuccess(response) {
			message := "unexpected AT response"
			if probeErr != nil {
				message = probeErr.Error()
			} else if strings.TrimSpace(response) != "" {
				message = fmt.Sprintf("unexpected response %q", response)
			}
			result.Commands = append(result.Commands, commandResult{
				Command:    "AT",
				ReturnCode: -1,
				Response:   trim(response),
				Error:      fmt.Sprintf("interface %d: %s", candidate.iface, message),
			})
			C.libusb_release_interface(handle, C.int(candidate.iface))
			continue
		}

		selected := candidate.public()
		result.Selected = &selected
		result.Commands = append(result.Commands, commandResult{Command: "AT", ReturnCode: 0, Response: trim(response)})
		for _, command := range readonlyCommands[1:] {
			response, err := sendCommand(handle, candidate, command)
			item := commandResult{Command: command, ReturnCode: 0, Response: trim(response)}
			if err != nil {
				item.ReturnCode = -1
				item.Error = err.Error()
			}
			result.Commands = append(result.Commands, item)
		}
		C.libusb_release_interface(handle, C.int(candidate.iface))
		result.Status = "READ_ONLY_AT_CAPTURED"
		return result
	}

	result.Status = "BLOCKED_AT_UNAVAILABLE"
	if result.Error == "" {
		result.Error = "bulk vendor interfaces were found, but none accepted a read-only AT probe"
	}
	return result
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
				candidates = append(candidates, rawCandidate{
					iface:    int(alt.bInterfaceNumber),
					class:    byte(alt.bInterfaceClass),
					subclass: byte(alt.bInterfaceSubClass),
					protocol: byte(alt.bInterfaceProtocol),
					in:       in,
					out:      out,
				})
			}
		}
	}
	// QDC507 research identifies interfaces 2/3 as AT/modem candidates. Try
	// those first, while retaining descriptor-driven fallback for variants.
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
