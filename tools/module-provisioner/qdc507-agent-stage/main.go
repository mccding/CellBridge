//go:build darwin && cgo

// qdc507-agent-stage is an explicitly write-capable, transient H2 bring-up
// tool. It is deliberately separate from qdc507-adb-readonly: it can only
// touch the fixed /data/cellbridge-agent-h2-test directory, never /etc, USB
// composition, boot images, or NAS state. The qmi-capture stage is a
// non-business protocol probe: its preload shim blocks WMS Raw Send before
// the modem and only records the C structure supplied by qmi_simple_ril_test.
package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdlib.h>
*/
import "C"

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
	"unsafe"
)

const (
	vendorID  = 0x2c7c
	productID = 0x0125

	adbCNXN = 0x4e584e43
	adbAUTH = 0x48545541
	adbOPEN = 0x4e45504f
	adbOKAY = 0x59414b4f
	adbCLSE = 0x45534c43
	adbWRTE = 0x45545257

	syncSEND = 0x444e4553
	syncDATA = 0x41544144
	syncDONE = 0x454e4f44
	syncOKAY = 0x59414b4f
	syncFAIL = 0x4c494146

	adbVersion  = 0x01000000
	adbMaxData  = 4096
	headSize    = 24
	packetLimit = 1024 * 1024
	ioTimeout   = 1500
	adbTimeout  = 40 * time.Second

	maxAgentSize = 16 * 1024 * 1024
	stageDir     = "/data/cellbridge-agent-h2-test"
	stageBinary  = stageDir + "/cellbridge-agent"
	stagePID     = stageDir + "/agent.pid"
	stageLog     = stageDir + "/agent.log"
	agentAddr    = "192.168.225.1:8788"
	agentService = "192.168.225.1"
	agentRIL     = "/usr/bin/qmi_simple_ril_test"
)

type packet struct {
	Command uint32
	Arg0    uint32
	Arg1    uint32
	Payload []byte
}

type shellResult struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	ReturnCode int    `json:"returnCode"`
	Stdout     string `json:"stdout,omitempty"`
	Error      string `json:"error,omitempty"`
}

type evidence struct {
	Schema       string        `json:"schema"`
	CapturedAt   string        `json:"capturedAt"`
	ReadOnly     bool          `json:"readOnly"`
	Status       string        `json:"status"`
	Stage        string        `json:"stage"`
	LocalBinary  string        `json:"localBinary,omitempty"`
	LocalSHA256  string        `json:"localSha256,omitempty"`
	RemotePath   string        `json:"remotePath,omitempty"`
	DeviceID     string        `json:"deviceId,omitempty"`
	AgentToken   string        `json:"agentToken,omitempty"`
	ADBInterface string        `json:"adbInterface,omitempty"`
	Commands     []shellResult `json:"commands,omitempty"`
	Error        string        `json:"error,omitempty"`
}

type usbADB struct {
	context *C.libusb_context
	handle  *C.libusb_device_handle
	iface   int
	in      byte
	out     byte
}

var errADBAuthRequired = errors.New("ADB device requested AUTH; no key or authorization was attempted")

func main() {
	stage := flag.String("stage", "", "required stage: start or cleanup")
	binaryPath := flag.String("binary", "", "local ARMv7 Agent binary for --stage=start")
	output := flag.String("output", "", "JSON evidence output path")
	flag.Parse()
	if *stage != "start" && *stage != "status" && *stage != "cleanup" && *stage != "qmi-capture" {
		fatal("--stage must be start, status, cleanup, or qmi-capture")
	}
	if strings.TrimSpace(*output) == "" {
		fatal("--output is required")
	}
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-agent-stage.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   *stage == "status",
		Status:     "UNKNOWN",
		Stage:      *stage,
	}
	if *stage == "start" {
		result.LocalBinary = *binaryPath
		result = startAgent(result, *binaryPath)
	} else if *stage == "status" {
		result = statusAgent(result)
	} else if *stage == "qmi-capture" {
		result.LocalBinary = *binaryPath
		result = captureQMIRequest(result, *binaryPath)
	} else {
		result = cleanupAgent(result)
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal("encode evidence: %v", err)
	}
	if err := os.WriteFile(*output, append(data, '\n'), 0o600); err != nil {
		fatal("write evidence: %v", err)
	}
	status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
	fmt.Println(string(status))
	if result.Status == "UNKNOWN" || strings.HasPrefix(result.Status, "BLOCKED_") {
		os.Exit(2)
	}
}

func startAgent(result evidence, localPath string) evidence {
	info, err := os.Stat(localPath)
	if err != nil {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = err.Error()
		return result
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAgentSize {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = fmt.Sprintf("Agent must be a non-empty regular file no larger than %d bytes", maxAgentSize)
		return result
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = err.Error()
		return result
	}
	digest := sha256.Sum256(data)
	result.LocalSHA256 = hex.EncodeToString(digest[:])
	deviceID, token, err := newCredentials()
	if err != nil {
		result.Status = "BLOCKED_CREDENTIAL_GENERATION"
		result.Error = err.Error()
		return result
	}
	result.DeviceID, result.AgentToken, result.RemotePath = deviceID, token, stageBinary

	device, err := connect()
	if err != nil {
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	result.ADBInterface = fmt.Sprintf("interface=%d bulk-in=0x%02x bulk-out=0x%02x", device.iface, device.in, device.out)
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	identity, err := device.shell("id")
	result.Commands = append(result.Commands, shellResult{Name: "id", Command: "id", ReturnCode: returnCode(err), Stdout: trim(identity), Error: errorText(err)})
	if err != nil || !strings.Contains(identity, "uid=0") {
		result.Status = "BLOCKED_NON_ROOT_SHELL"
		result.Error = "transient Agent stage requires a verified root shell"
		return result
	}
	check, err := device.shell(fmt.Sprintf("if [ -e %s ]; then echo STAGE_DIR_EXISTS=1; else echo STAGE_DIR_EXISTS=0; fi; df -k /data", shellQuote(stageDir)))
	result.Commands = append(result.Commands, shellResult{Name: "stage_preflight", Command: "fixed stage directory and /data free space", ReturnCode: returnCode(err), Stdout: trim(check), Error: errorText(err)})
	if err != nil || strings.Contains(check, "STAGE_DIR_EXISTS=1") {
		result.Status = "BLOCKED_STAGE_DIR_EXISTS"
		result.Error = "fixed transient stage directory is not empty; run cleanup first"
		return result
	}
	mkdirCommand := fmt.Sprintf("mkdir -m 700 %s", shellQuote(stageDir))
	mkdirOutput, err := device.shell(mkdirCommand)
	result.Commands = append(result.Commands, shellResult{Name: "mkdir_stage", Command: mkdirCommand, ReturnCode: returnCode(err), Stdout: trim(mkdirOutput), Error: errorText(err)})
	if err != nil {
		result.Status = "BLOCKED_STAGE_DIRECTORY"
		result.Error = err.Error()
		return result
	}
	if err := device.push(data, stageBinary); err != nil {
		result.Status = "BLOCKED_AGENT_PUSH"
		result.Error = err.Error()
		return result
	}
	startCommand := startCommand(token, deviceID)
	startOutput, err := device.shell(startCommand)
	result.Commands = append(result.Commands, shellResult{Name: "start_agent", Command: "fixed transient Agent start", ReturnCode: returnCode(err), Stdout: trim(startOutput), Error: errorText(err)})
	if err != nil || !strings.Contains(startOutput, "AGENT_RUNNING=1") {
		result.Status = "BLOCKED_AGENT_NOT_RUNNING"
		result.Error = "Agent did not remain alive after launch; inspect the staged log before cleanup"
		return result
	}
	result.Status = "H2_AGENT_STAGED"
	return result
}

func cleanupAgent(result evidence) evidence {
	device, err := connect()
	if err != nil {
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	command := fmt.Sprintf("pid=$(cat %s 2>/dev/null); case \"$pid\" in ''|*[!0-9]*) ;; *) kill \"$pid\" 2>/dev/null || true; sleep 1 ;; esac; for probe_pid in $(pidof qmi_simple_ril_test 2>/dev/null); do kill \"$probe_pid\" 2>/dev/null || true; done; sleep 1; rm -f %s %s %s; rmdir %s 2>/dev/null || true; if [ -e %s ] || [ -e %s ] || [ -e %s ]; then echo CLEANUP_REMAINING=1; else echo CLEANUP_REMAINING=0; fi", stagePID, stageBinary, stagePID, stageLog, stageDir, stageBinary, stagePID, stageLog)
	output, err := device.shell(command)
	result.Commands = append(result.Commands, shellResult{Name: "cleanup_fixed_stage", Command: "kill staged pid and remove fixed files", ReturnCode: returnCode(err), Stdout: trim(output), Error: errorText(err)})
	if err != nil || !strings.Contains(output, "CLEANUP_REMAINING=0") {
		result.Status = "BLOCKED_STAGE_CLEANUP"
		result.Error = "fixed transient stage directory was not fully cleaned"
		return result
	}
	result.Status = "H2_AGENT_STAGE_CLEANED"
	return result
}

func statusAgent(result evidence) evidence {
	device, err := connect()
	if err != nil {
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	command := fmt.Sprintf("echo ==== files; ls -la %s; echo ==== tools; command -v start-stop-daemon; command -v nohup; command -v setsid; command -v pidof; echo ==== pid; pid=$(cat %s 2>/dev/null); echo PID=$pid; echo ==== ps; ps; echo ==== log; tail -c 12000 %s 2>&1", stageDir, stagePID, stageLog)
	output, err := device.shell(command)
	result.Commands = append(result.Commands, shellResult{Name: "status_fixed_stage", Command: "read fixed transient stage files, process list, and log", ReturnCode: returnCode(err), Stdout: trim(output), Error: errorText(err)})
	if err != nil {
		result.Status = "BLOCKED_STAGE_STATUS"
		result.Error = err.Error()
		return result
	}
	result.Status = "H2_AGENT_STATUS_READ"
	return result
}

func captureQMIRequest(result evidence, localPath string) evidence {
	info, err := os.Stat(localPath)
	if err != nil {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = err.Error()
		return result
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxAgentSize {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = fmt.Sprintf("capture shim must be a non-empty regular file no larger than %d bytes", maxAgentSize)
		return result
	}
	data, err := os.ReadFile(localPath)
	if err != nil {
		result.Status = "BLOCKED_LOCAL_BINARY"
		result.Error = err.Error()
		return result
	}
	result.LocalSHA256 = sha256Hex(data)

	device, err := connect()
	if err != nil {
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	result.ADBInterface = fmt.Sprintf("interface=%d bulk-in=0x%02x bulk-out=0x%02x", device.iface, device.in, device.out)
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	identity, err := device.shell("id")
	result.Commands = append(result.Commands, shellResult{Name: "id", Command: "id", ReturnCode: returnCode(err), Stdout: trim(identity), Error: errorText(err)})
	if err != nil || !strings.Contains(identity, "uid=0") {
		result.Status = "BLOCKED_NON_ROOT_SHELL"
		result.Error = "QMI capture requires a verified root shell"
		return result
	}
	preflight, err := device.shell(fmt.Sprintf("if [ -e %s ]; then echo STAGE_DIR_EXISTS=1; else echo STAGE_DIR_EXISTS=0; fi", shellQuote(stageDir)))
	result.Commands = append(result.Commands, shellResult{Name: "capture_preflight", Command: "fixed transient stage directory preflight", ReturnCode: returnCode(err), Stdout: trim(preflight), Error: errorText(err)})
	if err != nil || strings.Contains(preflight, "STAGE_DIR_EXISTS=1") {
		result.Status = "BLOCKED_STAGE_DIR_EXISTS"
		result.Error = "fixed transient stage directory is not empty; run cleanup first"
		return result
	}
	mkdirCommand := fmt.Sprintf("mkdir -m 700 %s", shellQuote(stageDir))
	mkdirOutput, err := device.shell(mkdirCommand)
	result.Commands = append(result.Commands, shellResult{Name: "mkdir_capture_stage", Command: mkdirCommand, ReturnCode: returnCode(err), Stdout: trim(mkdirOutput), Error: errorText(err)})
	if err != nil {
		result.Status = "BLOCKED_STAGE_DIRECTORY"
		result.Error = err.Error()
		return result
	}
	if err := device.push(data, stageBinary); err != nil {
		result.Status = "BLOCKED_CAPTURE_SHIM_PUSH"
		result.Error = err.Error()
		return result
	}

	const pdu = "0001000B81815511118865F50008044F60597D"
	captureCommand := fmt.Sprintf("{ sleep 5; printf 'mo_sms_gsm %s\\n'; sleep 8; printf 'quit\\n'; } | /bin/timeout -t 25 env LD_PRELOAD=%s /usr/bin/qmi_simple_ril_test >%s 2>&1; rc=$?; echo CAPTURE_RC=$rc; echo ==== CAPTURE_LOG; tail -c 30000 %s 2>&1; rm -f %s %s %s; rmdir %s 2>/dev/null || true; if [ -e %s ] || [ -e %s ] || [ -e %s ]; then echo CAPTURE_CLEANUP_REMAINING=1; else echo CAPTURE_CLEANUP_REMAINING=0; fi", pdu, shellQuote(stageBinary), shellQuote(stageLog), shellQuote(stageLog), shellQuote(stageBinary), shellQuote(stagePID), shellQuote(stageLog), shellQuote(stageDir), shellQuote(stageBinary), shellQuote(stagePID), shellQuote(stageLog))
	output, err := device.shell(captureCommand)
	result.Commands = append(result.Commands, shellResult{Name: "capture_wms_raw_send_request", Command: "LD_PRELOAD capture; WMS Raw Send is blocked before modem", ReturnCode: returnCode(err), Stdout: trim(output), Error: errorText(err)})
	if err != nil || !strings.Contains(output, "CAPTURE_CLEANUP_REMAINING=0") {
		result.Status = "BLOCKED_CAPTURE_CLEANUP"
		result.Error = "QMI capture did not prove cleanup of the fixed transient directory"
		return result
	}
	if !strings.Contains(output, "[CELLBRIDGE_QMI_CAPTURE] modem_send=blocked") {
		result.Status = "BLOCKED_QMI_REQUEST_CAPTURE"
		result.Error = "preload shim did not intercept the expected WMS Raw Send call"
		return result
	}
	result.Status = "H2_QMI_REQUEST_CAPTURED"
	return result
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func newCredentials() (string, string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", "", err
	}
	hexValue := hex.EncodeToString(value[:])
	return "qdc507-h2-" + hexValue[:16], "cb-h2-" + hexValue, nil
}

func startCommand(token, deviceID string) string {
	return fmt.Sprintf("nohup env CB_AGENT_BIND_ADDR=%s CB_AGENT_SERVICE_IP=%s CB_AGENT_SERVICE_NAME=%s CB_AGENT_DEVICE_ID=%s CB_AGENT_TOKEN=%s CB_AGENT_RIL_PATH=%s CB_AGENT_ENABLE_VERIFIED_CALL=1 CB_AGENT_ENABLE_VERIFIED_SMS=1 %s >%s 2>&1 </dev/null & echo $! >%s; sleep 1; pid=$(cat %s 2>/dev/null); case \"$pid\" in ''|*[!0-9]*) echo AGENT_PID_INVALID;; *) kill -0 \"$pid\" 2>/dev/null && echo AGENT_RUNNING=1 || echo AGENT_RUNNING=0;; esac", shellQuote(agentAddr), shellQuote(agentService), shellQuote("CellBridge QDC507"), shellQuote(deviceID), shellQuote(token), shellQuote(agentRIL), shellQuote(stageBinary), shellQuote(stageLog), shellQuote(stagePID), shellQuote(stagePID))
}

func connect() (*usbADB, error) {
	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		return nil, fmt.Errorf("libusb init: %s", usbError(rc))
	}
	device, err := openADBDevice(context)
	if err != nil {
		C.libusb_exit(context)
		return nil, err
	}
	return device, nil
}

func openADBDevice(context *C.libusb_context) (*usbADB, error) {
	handle := C.libusb_open_device_with_vid_pid(context, C.uint16_t(vendorID), C.uint16_t(productID))
	if handle == nil {
		return nil, errors.New("QDC507 USB device 2c7c:0125 was not found by libusb")
	}
	device := C.libusb_get_device(handle)
	if device == nil {
		C.libusb_close(handle)
		return nil, errors.New("libusb returned no device for QDC507")
	}
	var config *C.struct_libusb_config_descriptor
	if rc := C.libusb_get_active_config_descriptor(device, &config); rc != 0 {
		C.libusb_close(handle)
		return nil, fmt.Errorf("get active configuration: %s", usbError(rc))
	}
	defer C.libusb_free_config_descriptor(config)
	interfaces := unsafe.Slice(config._interface, int(config.bNumInterfaces))
	for _, intf := range interfaces {
		altSettings := unsafe.Slice(intf.altsetting, int(intf.num_altsetting))
		for _, alt := range altSettings {
			if byte(alt.bInterfaceClass) != 0xff || byte(alt.bInterfaceSubClass) != 0x42 || byte(alt.bInterfaceProtocol) != 0x01 {
				continue
			}
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
			if in == 0 || out == 0 {
				continue
			}
			iface := int(alt.bInterfaceNumber)
			if rc := C.libusb_claim_interface(handle, C.int(iface)); rc != 0 {
				C.libusb_close(handle)
				return nil, fmt.Errorf("claim ADB interface %d: %s", iface, usbError(rc))
			}
			return &usbADB{context: context, handle: handle, iface: iface, in: in, out: out}, nil
		}
	}
	C.libusb_close(handle)
	return nil, errors.New("QDC507 has no ff/42/01 bulk ADB interface")
}

func (d *usbADB) close() {
	if d == nil || d.handle == nil {
		return
	}
	C.libusb_release_interface(d.handle, C.int(d.iface))
	C.libusb_close(d.handle)
	C.libusb_exit(d.context)
	d.handle = nil
	d.context = nil
}

func (d *usbADB) handshake() error {
	if err := d.writePacket(packet{Command: adbCNXN, Arg0: adbVersion, Arg1: adbMaxData, Payload: []byte("host::\x00")}); err != nil {
		return err
	}
	response, err := d.readPacket(adbTimeout)
	if err != nil {
		return fmt.Errorf("read CNXN response: %w", err)
	}
	switch response.Command {
	case adbCNXN:
		return nil
	case adbAUTH:
		return errADBAuthRequired
	default:
		return fmt.Errorf("unexpected ADB handshake command 0x%08x", response.Command)
	}
}

func (d *usbADB) shell(command string) (string, error) {
	const localID = 1
	if err := d.writePacket(packet{Command: adbOPEN, Arg0: localID, Payload: append([]byte("shell:"+command), 0)}); err != nil {
		return "", fmt.Errorf("open shell: %w", err)
	}
	var remoteID uint32
	var output strings.Builder
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return output.String(), fmt.Errorf("read shell: %w", err)
		}
		switch response.Command {
		case adbOKAY:
			remoteID = response.Arg0
		case adbWRTE:
			if remoteID == 0 {
				remoteID = response.Arg0
			}
			output.Write(response.Payload)
			if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: remoteID}); err != nil {
				return output.String(), err
			}
		case adbCLSE:
			_ = d.writePacket(packet{Command: adbCLSE, Arg0: localID, Arg1: response.Arg0})
			return output.String(), nil
		default:
			return output.String(), fmt.Errorf("unexpected shell packet 0x%08x", response.Command)
		}
	}
	return output.String(), errors.New("shell timed out")
}

func (d *usbADB) push(data []byte, remotePath string) error {
	if !strings.HasPrefix(remotePath, stageDir+"/") || strings.ContainsRune(remotePath, 0) {
		return errors.New("push path is outside the fixed transient stage directory")
	}
	if len(data) == 0 || len(data) > maxAgentSize {
		return errors.New("Agent binary has an invalid size")
	}
	const localID = 2
	remoteID, err := d.openService(localID, "sync:")
	if err != nil {
		return err
	}
	defer func() { _ = d.writePacket(packet{Command: adbCLSE, Arg0: localID, Arg1: remoteID}) }()
	name := []byte(remotePath + ",0700")
	if err := d.sendSyncRecord(localID, remoteID, syncSEND, name); err != nil {
		return fmt.Errorf("send ADB SEND: %w", err)
	}
	const chunk = adbMaxData - 8
	for offset := 0; offset < len(data); {
		end := offset + chunk
		if end > len(data) {
			end = len(data)
		}
		if err := d.sendSyncRecord(localID, remoteID, syncDATA, data[offset:end]); err != nil {
			return fmt.Errorf("send ADB DATA at %d: %w", offset, err)
		}
		offset = end
	}
	done := make([]byte, 4)
	binary.LittleEndian.PutUint32(done, uint32(time.Now().Unix()))
	if err := d.sendSyncRecord(localID, remoteID, syncDONE, done); err != nil {
		return fmt.Errorf("send ADB DONE: %w", err)
	}
	if err := d.readSyncResult(localID, remoteID); err != nil {
		return err
	}
	return d.closeService(localID, remoteID)
}

func (d *usbADB) sendSyncRecord(localID, remoteID, kind uint32, data []byte) error {
	record := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(record[0:4], kind)
	binary.LittleEndian.PutUint32(record[4:8], uint32(len(data)))
	copy(record[8:], data)
	if err := d.writePacket(packet{Command: adbWRTE, Arg0: localID, Arg1: remoteID, Payload: record}); err != nil {
		return err
	}
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return err
		}
		switch response.Command {
		case adbOKAY:
			return nil
		case adbWRTE:
			if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: response.Arg0}); err != nil {
				return err
			}
		case adbCLSE:
			return errors.New("ADB sync service closed while sending")
		default:
			return fmt.Errorf("unexpected ADB sync acknowledgement 0x%08x", response.Command)
		}
	}
	return errors.New("ADB sync acknowledgement timed out")
}

func (d *usbADB) readSyncResult(localID, remoteID uint32) error {
	var buffer []byte
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return fmt.Errorf("read ADB sync result: %w", err)
		}
		if response.Command == adbOKAY {
			continue
		}
		if response.Command == adbCLSE {
			return errors.New("ADB sync service closed before result")
		}
		if response.Command != adbWRTE {
			return fmt.Errorf("unexpected ADB sync result packet 0x%08x", response.Command)
		}
		if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: response.Arg0}); err != nil {
			return err
		}
		buffer = append(buffer, response.Payload...)
		for len(buffer) >= 8 {
			kind := binary.LittleEndian.Uint32(buffer[0:4])
			length := binary.LittleEndian.Uint32(buffer[4:8])
			if length > packetLimit || int(length)+8 > len(buffer) {
				break
			}
			payload := buffer[8 : 8+int(length)]
			buffer = buffer[8+int(length):]
			switch kind {
			case syncOKAY:
				return nil
			case syncFAIL:
				return fmt.Errorf("module rejected ADB push: %s", string(payload))
			default:
				return fmt.Errorf("unexpected ADB sync result 0x%08x", kind)
			}
		}
	}
	return errors.New("ADB sync result timed out")
}

func (d *usbADB) openService(localID uint32, service string) (uint32, error) {
	if err := d.writePacket(packet{Command: adbOPEN, Arg0: localID, Payload: append([]byte(service), 0)}); err != nil {
		return 0, fmt.Errorf("open ADB %s: %w", service, err)
	}
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return 0, err
		}
		switch response.Command {
		case adbOKAY:
			return response.Arg0, nil
		case adbCLSE:
			return 0, fmt.Errorf("ADB %s service closed during open", service)
		default:
			return 0, fmt.Errorf("unexpected ADB %s open packet 0x%08x", service, response.Command)
		}
	}
	return 0, fmt.Errorf("ADB %s open timed out", service)
}

func (d *usbADB) closeService(localID, remoteID uint32) error {
	if err := d.writePacket(packet{Command: adbCLSE, Arg0: localID, Arg1: remoteID}); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return err
		}
		switch response.Command {
		case adbCLSE:
			return nil
		case adbOKAY:
			continue
		case adbWRTE:
			if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: response.Arg0}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected ADB close packet 0x%08x", response.Command)
		}
	}
	return errors.New("ADB service close timed out")
}

func (d *usbADB) writePacket(message packet) error {
	if len(message.Payload) > packetLimit {
		return errors.New("ADB payload exceeds safety limit")
	}
	header := make([]byte, headSize)
	binary.LittleEndian.PutUint32(header[0:4], message.Command)
	binary.LittleEndian.PutUint32(header[4:8], message.Arg0)
	binary.LittleEndian.PutUint32(header[8:12], message.Arg1)
	binary.LittleEndian.PutUint32(header[12:16], uint32(len(message.Payload)))
	binary.LittleEndian.PutUint32(header[16:20], checksum(message.Payload))
	binary.LittleEndian.PutUint32(header[20:24], message.Command^0xffffffff)
	if err := d.bulkWrite(header); err != nil {
		return err
	}
	if len(message.Payload) > 0 {
		return d.bulkWrite(message.Payload)
	}
	return nil
}

func (d *usbADB) readPacket(timeout time.Duration) (packet, error) {
	header, err := d.bulkRead(headSize, timeout)
	if err != nil {
		return packet{}, err
	}
	if len(header) != headSize {
		return packet{}, fmt.Errorf("short ADB header: %d/%d bytes", len(header), headSize)
	}
	message := packet{Command: binary.LittleEndian.Uint32(header[0:4]), Arg0: binary.LittleEndian.Uint32(header[4:8]), Arg1: binary.LittleEndian.Uint32(header[8:12])}
	length := binary.LittleEndian.Uint32(header[12:16])
	if length > packetLimit {
		return packet{}, fmt.Errorf("ADB payload length %d exceeds safety limit", length)
	}
	if message.Command^0xffffffff != binary.LittleEndian.Uint32(header[20:24]) {
		return packet{}, errors.New("ADB command magic mismatch")
	}
	if length == 0 {
		return message, nil
	}
	payload, err := d.bulkRead(int(length), timeout)
	if err != nil {
		return packet{}, err
	}
	if checksum(payload) != binary.LittleEndian.Uint32(header[16:20]) {
		return packet{}, errors.New("ADB payload checksum mismatch")
	}
	message.Payload = payload
	return message, nil
}

func (d *usbADB) bulkWrite(payload []byte) error {
	buffer := C.CBytes(payload)
	defer C.free(buffer)
	transferred := C.int(0)
	rc := C.libusb_bulk_transfer(d.handle, C.uchar(d.out), (*C.uchar)(buffer), C.int(len(payload)), &transferred, C.uint(ioTimeout))
	if rc != 0 {
		return fmt.Errorf("ADB bulk write: %s", usbError(rc))
	}
	if int(transferred) != len(payload) {
		return fmt.Errorf("ADB bulk write short transfer: %d/%d bytes", transferred, len(payload))
	}
	return nil
}

func (d *usbADB) bulkRead(size int, timeout time.Duration) ([]byte, error) {
	if size <= 0 || size > packetLimit {
		return nil, errors.New("invalid ADB read size")
	}
	timeoutMS := int(timeout.Milliseconds())
	if timeoutMS < 1 {
		timeoutMS = 1
	}
	buffer := C.malloc(C.size_t(size))
	defer C.free(buffer)
	transferred := C.int(0)
	rc := C.libusb_bulk_transfer(d.handle, C.uchar(d.in), (*C.uchar)(buffer), C.int(size), &transferred, C.uint(timeoutMS))
	if rc == C.LIBUSB_ERROR_TIMEOUT {
		return nil, errors.New("ADB USB transfer timed out")
	}
	if rc != 0 {
		return nil, fmt.Errorf("ADB bulk read: %s", usbError(rc))
	}
	return C.GoBytes(buffer, transferred), nil
}

func checksum(payload []byte) uint32 {
	var total uint32
	for _, value := range payload {
		total += uint32(value)
	}
	return total
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func trim(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) > 16384 {
		return value[:16384] + "\n...[truncated]"
	}
	return value
}

func returnCode(err error) int {
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

func statusForOpenError(err error) string {
	if strings.Contains(err.Error(), "not found") {
		return "BLOCKED_QDC507_NOT_FOUND"
	}
	if strings.Contains(err.Error(), "no ff/42/01") {
		return "BLOCKED_NO_ADB_INTERFACE"
	}
	return "BLOCKED_ADB_OPEN"
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
