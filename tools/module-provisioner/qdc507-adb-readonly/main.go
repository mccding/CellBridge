//go:build darwin && cgo

// qdc507-adb-readonly is a clean-room USB ADB probe for v8.
// The default and all probe modes only read module state. A separately gated
// --hil-dial/--hil-hangup mode exists for explicitly authorized real-call HIL;
// it never calls adb root, sync push, reboot, USBCFG, or usbnet commands.
package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdlib.h>
*/
import "C"

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

	syncRECV = 0x56434552
	syncDATA = 0x41544144
	syncDONE = 0x454e4f44
	syncFAIL = 0x4c494146

	adbVersion  = 0x01000000
	adbMaxData  = 4096
	headSize    = 24
	packetLimit = 1024 * 1024
	ioTimeout   = 1500
	adbTimeout  = 40 * time.Second
)

var readCommands = []struct {
	Name    string
	Command string
}{
	{Name: "id", Command: "id"},
	{Name: "uname", Command: "uname -a"},
	{Name: "proc_cmdline", Command: "cat /proc/cmdline"},
	{Name: "mounts", Command: "mount"},
	{Name: "disk_free", Command: "df -k"},
	{Name: "processes", Command: "ps"},
	{Name: "control_process_fds", Command: "for name in qti qmuxd netmgrd atfwd_daemon quectel_daemon; do for p in $(pidof \"$name\" 2>/dev/null); do echo \"[$name pid=$p]\"; ls -l /proc/$p/fd; done; done"},
	{Name: "dev", Command: "ls -l /dev"},
	{Name: "dev_smd", Command: "ls -l /dev/smd*"},
	{Name: "dev_tty", Command: "ls -l /dev/tty*"},
	{Name: "dev_diag", Command: "ls -l /dev/diag*"},
	{Name: "dev_qmi", Command: "ls -l /dev/qmi*"},
	{Name: "dev_cdc", Command: "ls -l /dev/cdc*"},
	{Name: "internal_at_tools", Command: "ls -l /dev/at_usb0 /dev/at_usb1 2>&1; command -v timeout 2>&1; command -v busybox 2>&1; command -v toybox 2>&1; command -v stty 2>&1"},
	{Name: "internal_at_timeout_help", Command: "/bin/timeout --help 2>&1 | head -40"},
	{Name: "native_build_tools", Command: `for f in /usr/bin/cc /usr/bin/gcc /usr/bin/clang /usr/bin/make /usr/bin/ld /usr/bin/objcopy /usr/bin/strip; do if [ -e "$f" ]; then ls -l "$f"; fi; done; find /usr /opt /data -maxdepth 4 -type f \( -name 'cc' -o -name 'gcc' -o -name 'clang' -o -name 'ld' \) -perm -111 2>/dev/null | sort | head -80`},
	{Name: "sys_class", Command: "ls -l /sys/class"},
	{Name: "proc_devices", Command: "cat /proc/devices"},
	{Name: "proc_misc", Command: "cat /proc/misc"},
	{Name: "process_candidates", Command: "ps | grep -E 'rild|qmuxd|netmgrd|qmi|ims|voice|at|modem' | grep -v grep"},
	{Name: "unix_sockets", Command: "cat /proc/net/unix"},
	{Name: "netstat", Command: "command -v ss >/dev/null && ss -lntup || command -v netstat >/dev/null && netstat -lntup || true"},
}

var qmiReadCommands = []struct {
	Name    string
	Command string
}{
	{Name: "control_cmdlines", Command: "for p in 422 538 582 589 598 689; do if [ -r /proc/$p/cmdline ]; then printf '[pid=%s] ' $p; tr '\\000' ' ' < /proc/$p/cmdline; echo; fi; done"},
	{Name: "control_binaries", Command: "ls -l /bin /sbin /usr/bin /usr/sbin 2>/dev/null | grep -E 'qmi|ril|radio|at|modem|voice|sms|quectel|netmgr|qmux|diag' || true"},
	{Name: "control_strings", Command: "if command -v strings >/dev/null 2>&1; then for f in /usr/bin/qmuxd /usr/bin/netmgrd /usr/bin/atfwd_daemon /usr/bin/quectel_daemon; do echo [$f]; strings $f 2>/dev/null | grep -E 'socket|/dev/|qmi|QMI|ril|RIL|voice|sms|urc|netmgr' | head -80; done; fi"},
	{Name: "qti_strings", Command: "if command -v strings >/dev/null 2>&1; then strings /usr/bin/qti 2>/dev/null | grep -E 'socket|/dev/|qmi|QMI|ril|RIL|voice|sms|urc|rmnet' | head -180; fi"},
	{Name: "qmi_config", Command: "for f in /etc/data/qmi_config.xml /data/qmi_ip_cfg.xml /var/qmux_connect_socket; do echo [$f]; ls -l $f 2>&1; if [ -f $f ]; then sed -n '1,220p' $f; fi; done"},
	{Name: "qmi_config_matches", Command: "grep -n -E 'mdm|msm|8909|9607|smdcntl|rmnet|QMI_PORT|QMI_CONN' /etc/data/qmi_config.xml | tail -180"},
	{Name: "qmi_artifacts", Command: "find /etc /data /usr -maxdepth 4 -iname '*qmi*' -o -iname '*ril*' 2>/dev/null | sort | head -240"},
	{Name: "qmi_runtime_fds", Command: "( sleep 20 | /usr/bin/qmi_simple_ril_test >/dev/null 2>&1 ) & probe_job=$!; sleep 5; for probe_pid in $(pidof qmi_simple_ril_test 2>/dev/null); do echo [qmi_simple_ril_test pid=$probe_pid]; ls -l /proc/$probe_pid/fd 2>&1; echo ==== maps; cat /proc/$probe_pid/maps 2>&1 | grep -E 'libqmi|qmi_simple' || true; done; echo ==== unix_during_qmi; cat /proc/net/unix; kill $probe_job 2>/dev/null || true; for probe_pid in $(pidof qmi_simple_ril_test 2>/dev/null); do kill $probe_pid 2>/dev/null || true; done; wait $probe_job 2>/dev/null || true"},
	{Name: "qmi_test_strings", Command: "if command -v strings >/dev/null 2>&1; then strings /usr/bin/qmi_simple_ril_test 2>/dev/null | grep -E 'usage|Usage|help|socket|qmi|QMI|UIM|NAS|WMS|VOICE|SMS|call|dial|AT' | head -180; fi"},
	{Name: "qmi_test_commands", Command: "if command -v strings >/dev/null 2>&1; then strings /usr/bin/qmi_simple_ril_test 2>/dev/null | grep -E '^[A-Za-z][A-Za-z0-9_ -]{1,48}$' | grep -E '(^help$|^dial$|^call_|^sms|^send|^get_|^set_|^hang|^answer|^reject|^dtmf|^uim|^nas|^wms|^voice|^dms|^qmi)' | sort -u | head -240; fi"},
	{Name: "qmi_test_help", Command: "{ sleep 5; printf '?\\n'; sleep 3; printf 'help\\n'; sleep 3; printf 'quit\\n'; } | /bin/timeout -t 20 -s KILL /usr/bin/qmi_simple_ril_test 2>&1"},
	{Name: "qmi_safe_argument_probe", Command: "{ sleep 5; printf 'dial\\n'; sleep 2; printf 'mo_sms_gsm\\n'; sleep 2; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 18 -s KILL /usr/bin/qmi_simple_ril_test 2>&1"},
	{Name: "qmi_test_usage_context", Command: "if command -v strings >/dev/null 2>&1; then n=$(strings /usr/bin/qmi_simple_ril_test 2>/dev/null | grep -n '^get_imsi$' | head -1 | cut -d: -f1); if [ -n \"$n\" ]; then start=$((n-35)); [ $start -lt 1 ] && start=1; end=$((n+180)); strings /usr/bin/qmi_simple_ril_test 2>/dev/null | sed -n \"${start},${end}p\"; fi; fi"},
	{Name: "qmi_readonly_console", Command: "{ sleep 5; echo '[CMD modem status]' >&2; printf 'modem status\\n'; sleep 2; echo '[CMD get_dev_id]' >&2; printf 'get_dev_id\\n'; sleep 2; echo '[CMD get_imsi 0 1]' >&2; printf 'get_imsi 0 1\\n'; sleep 2; echo '[CMD get_imsi 1 0]' >&2; printf 'get_imsi 1 0\\n'; sleep 2; echo '[CMD get_imsi 1 1]' >&2; printf 'get_imsi 1 1\\n'; sleep 2; echo '[CMD card 0 0]' >&2; printf 'card 0 0\\n'; sleep 2; echo '[CMD card 0 1]' >&2; printf 'card 0 1\\n'; sleep 2; echo '[CMD card 1]' >&2; printf 'card 1\\n'; sleep 2; echo '[CMD nw_cdma_info]' >&2; printf 'nw_cdma_info\\n'; sleep 2; echo '[CMD call_state]' >&2; printf 'call_state\\n'; sleep 2; echo '[CMD qmi_svc_versions]' >&2; printf 'qmi_svc_versions\\n'; sleep 1; echo '[CMD quit]' >&2; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 35 -s KILL /usr/bin/qmi_simple_ril_test 2>&1"},
	{Name: "qmi_readonly_parser_forms", Command: "{ sleep 5; echo '[CMD modem status]' >&2; printf 'modem status\\n'; sleep 2; echo '[CMD [modem]status]' >&2; printf '[modem]status\\n'; sleep 2; echo '[CMD [modem] status]' >&2; printf '[modem] status\\n'; sleep 2; echo '[CMD [modem status]]' >&2; printf '[modem status]\\n'; sleep 2; echo '[CMD modem]' >&2; printf 'modem\\n'; sleep 1; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 20 /usr/bin/qmi_simple_ril_test 2>&1 | head -500"},
	{Name: "qmi_readonly_argument_forms", Command: "{ sleep 5; echo '[CMD get_imsi 0 1]' >&2; printf 'get_imsi 0 1\\n'; sleep 2; echo '[CMD [get_imsi] 0 1]' >&2; printf '[get_imsi] 0 1\\n'; sleep 2; echo '[CMD [get_imsi]0 1]' >&2; printf '[get_imsi]0 1\\n'; sleep 2; echo '[CMD get_imsi [0] [1]]' >&2; printf 'get_imsi [0] [1]\\n'; sleep 1; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 22 /usr/bin/qmi_simple_ril_test 2>&1 | head -600"},
	{Name: "qmi_test_argument_strings", Command: "if command -v strings >/dev/null 2>&1; then strings /usr/bin/qmi_simple_ril_test 2>/dev/null | grep -i -E 'usage|argument|invalid|card|imsi|status|nw_|call_state|service version' | head -260; fi"},
	{Name: "qmi_raw_ctl_probe", Command: "if [ ! -c /dev/smdcntl0 ]; then echo MISSING; else /bin/timeout -t 5 /bin/sh -c 'echo BEFORE; exec 3<>/dev/smdcntl0 || exit 1; echo OPENED; printf '\\001\\013\\000\\000\\000\\000\\000\\041\\000\\000\\000\\000' >&3; echo SENT; /bin/timeout -t 3 /bin/dd if=/dev/smdcntl0 bs=1 count=1024 <&3 2>/dev/null | /bin/od -An -tx1'; fi"},
	{Name: "qmi_logs", Command: "ls -l /data/logs 2>&1; grep -R -i -E 'qmi|voice|modem|atfwd|rmnet' /data/logs 2>/dev/null | tail -200"},
	{Name: "qmi_runtime_logs", Command: "echo ==== dmesg; dmesg 2>&1 | tail -240; echo ==== logread; if command -v logread >/dev/null 2>&1; then logread 2>&1 | tail -240; else echo LOGREAD_UNAVAILABLE; fi"},
	{Name: "modem_startup", Command: "for f in /etc/init.d/qmi_shutdown_modemd /etc/init.d/modem-shutdown /etc/rc2.d/S45qmi_shutdown_modemd /etc/rc3.d/S45qmi_shutdown_modemd /etc/rc4.d/S45qmi_shutdown_modemd /etc/rc5.d/S45qmi_shutdown_modemd; do echo [$f]; sed -n '1,220p' $f 2>&1; done"},
	{Name: "control_device_owners", Command: "for p in /proc/[0-9]*; do for f in $p/fd/*; do target=$(readlink $f 2>/dev/null); case $target in */dev/smdcntl*|*/dev/rmnet_ctrl*|*/dev/diag) echo $p $f $target;; esac; done; done"},
	{Name: "net_interfaces", Command: "cat /proc/net/dev; echo ROUTE; cat /proc/net/route; echo SOCKETS; ls -l /tmp /run /data 2>/dev/null | head -120"},
	{Name: "control_socket_paths", Command: "for path in /var /var/tmp /tmp /run /data; do echo [$path]; find $path -maxdepth 2 -type s -o -type p 2>/dev/null | sort; done"},
	{Name: "vendor_at_bridge_state", Command: "echo ==== processes; ps | grep -E 'atfwd|quectel-uart-ddp|uart_ddp|ipth_dme' | grep -v grep || true; echo ==== runtime; for p in /tmp/.urc_sock /tmp/atfwd_socket_server /tmp/atfwd_socket_client /tmp/atfwd_daemon_rdy /tmp/quec_uart_ddp_rdy /data/ipth_dme_urc /data/ipth_dme_cmd /data/quec/conf/dynamic_console; do ls -la $p 2>&1; done; echo ==== unix; cat /proc/net/unix | grep -E 'urc|atfwd|ipth|quec' || true; echo ==== uart_conf; for p in /data/quec_dbg_uart_conf/baudrate /data/quec_dbg_uart_conf/databits /data/quec_dbg_uart_conf/stopbits /data/quec_dbg_uart_conf/parity /data/quec_dbg_uart_conf/flowctrl /data/quec_dbg_uart_conf/debuginf; do echo [$p]; if [ -f $p ]; then sed -n '1,4p' $p; else ls -l $p 2>&1; fi; done"},
}

var m1ReadCommands = []struct {
	Name    string
	Command string
}{
	{Name: "m1_mounts", Command: "mount; echo PROC_MOUNTS; cat /proc/mounts; echo DF; df -k /data /usrdata /cache /tmp 2>&1"},
	{Name: "m1_persistent_paths", Command: "for p in /data /usrdata /cache /etc /etc/init.d /etc/rcS.d /etc/rc2.d /etc/rc3.d /etc/rc4.d /etc/rc5.d /system/bin /usr/bin; do ls -ld $p 2>&1; done"},
	{Name: "m1_init_entries", Command: "for p in /etc/inittab /etc/rcS /etc/rc.local /etc/init.d /etc/rcS.d /etc/rc2.d /etc/rc3.d /etc/rc4.d /etc/rc5.d; do echo [$p]; ls -la $p 2>&1 | head -160; done"},
	{Name: "m1_executable_paths", Command: "for p in /data /usrdata /cache /tmp; do if [ -d $p ]; then printf '[%s] ' $p; ls -ld $p; mountpoint -q $p 2>/dev/null && echo MOUNTPOINT || true; fi; done"},
	{Name: "m1_boot_script_sources", Command: "for p in /etc/inittab /etc/init.d/data-init /etc/init.d/rc /etc/init.d/rcS /etc/init.d/read-only-rootfs-hook.sh /etc/init.d/usb; do echo ==== $p; sed -n '1,260p' $p 2>&1; done"},
	{Name: "m1_data_inventory", Command: "for p in /data /usrdata; do echo ==== $p; find $p -maxdepth 3 -mindepth 1 -type d -o -type f -o -type l 2>/dev/null | sort | head -320; done"},
	{Name: "m1_startup_references", Command: "grep -R -n -E 'rcS|init.d|daemon|watchdog|usbnet|cellbridge|qti|quectel' /etc/init* /etc/rc* /etc/default /data 2>/dev/null | head -260"},
}

var m2ReadCommands = []struct {
	Name    string
	Command string
}{
	{Name: "m2_vendor_start_scripts", Command: "for p in /etc/init.d/start_ql_manager_server_le /etc/init.d/start_qti_le /etc/init.d/start_atfwd_daemon /etc/init.d/quectel_daemon /etc/init.d/netmgrd /etc/init.d/qmuxd /etc/init.d/rc; do echo ==== $p; sed -n '1,260p' $p 2>&1; done"},
	{Name: "m2_startup_data_refs", Command: "grep -R -n -E '/data|/usrdata|crond|cron|respawn|service|watchdog|start-stop-daemon' /etc/inittab /etc/init.d /etc/rc*.d /etc/default 2>/dev/null | head -400"},
	{Name: "m2_cron_state", Command: "for p in /var/spool/cron /var/spool/cron/crontabs /etc/crontab /etc/cron.d /etc/cron.daily; do echo ==== $p; ls -la $p 2>&1; done; crontab -l 2>&1 || true"},
	{Name: "m2_data_executables", Command: "find /data /usrdata -maxdepth 4 -type f -perm -111 2>/dev/null | sort | head -240"},
	{Name: "m2_manager_strings", Command: "if command -v strings >/dev/null 2>&1; then strings /usr/bin/ql_manager_server 2>/dev/null | grep -i -E 'data|script|service|start|watch|exec|command|config' | head -240; fi"},
}

const qmiReadOnlyConsole = "{ sleep 5; printf '?\\n'; sleep 2; printf 'help\\n'; sleep 2; printf 'modem status\\n'; sleep 2; printf 'get_dev_id\\n'; sleep 2; printf 'get_imsi 0 gw\\n'; sleep 2; printf 'get_imsi 0 1x\\n'; sleep 2; printf 'card gw\\n'; sleep 2; printf 'card 1x\\n'; sleep 2; printf 'nw_cdma_info\\n'; sleep 2; printf 'call_state\\n'; sleep 2; printf 'qmi_svc_versions\\n'; sleep 1; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 35 /usr/bin/qmi_simple_ril_test 2>&1"

var phoneNumberPattern = regexp.MustCompile(`^[0-9]{3,20}$`)
var smsPDUPattern = regexp.MustCompile(`^(?:[0-9A-Fa-f]{2})+$`)

const (
	persistenceStageDir    = "/data/.cellbridge-persist-probe"
	persistenceStageMarker = persistenceStageDir + "/marker"
	persistenceStageBinary = persistenceStageDir + "/busybox"
)

type adbInterface struct {
	Interface   int    `json:"interface"`
	Class       string `json:"class"`
	Subclass    string `json:"subclass"`
	Protocol    string `json:"protocol"`
	EndpointIn  string `json:"endpointIn"`
	EndpointOut string `json:"endpointOut"`
}

type shellResult struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	ReturnCode int    `json:"returnCode"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	Error      string `json:"error,omitempty"`
}

type evidence struct {
	Schema        string        `json:"schema"`
	CapturedAt    string        `json:"capturedAt"`
	ReadOnly      bool          `json:"readOnly"`
	VendorID      string        `json:"vendorId"`
	ProductID     string        `json:"productId"`
	Status        string        `json:"status"`
	ADBInterface  *adbInterface `json:"adbInterface,omitempty"`
	Handshake     string        `json:"handshake,omitempty"`
	ShellIdentity string        `json:"shellIdentity,omitempty"`
	ShellIsRoot   bool          `json:"shellIsRoot"`
	Commands      []shellResult `json:"commands,omitempty"`
	PulledFile    string        `json:"pulledFile,omitempty"`
	ProbeToken    string        `json:"probeToken,omitempty"`
	Error         string        `json:"error,omitempty"`
}

type packet struct {
	Command uint32
	Arg0    uint32
	Arg1    uint32
	Payload []byte
}

type usbADB struct {
	context *C.libusb_context
	handle  *C.libusb_device_handle
	iface   int
	in      byte
	out     byte
}

func main() {
	output := flag.String("output", "", "JSON evidence output path")
	probeInternalAT := flag.Bool("probe-internal-at", false, "query /dev/at_usb0 and /dev/at_usb1 with v8 read-only AT commands")
	probeQMI := flag.Bool("probe-qmi", false, "inspect the existing QMI/Simple-RIL control plane")
	probeQMILite := flag.Bool("probe-qmi-lite", false, "run one bounded read-only QMI/Simple-RIL status sample")
	probeM1 := flag.Bool("probe-m1", false, "inspect persistent paths and startup entries without writing the module")
	probeM2 := flag.Bool("probe-m2", false, "inspect reversible startup-hook candidates without writing the module")
	persistenceStage := flag.String("persistence-stage", "", "explicit M1 probe stage: create, verify, verify-reboot, or cleanup")
	persistenceToken := flag.String("persistence-token", "", "token returned by persistence-stage=create")
	pullSource := flag.String("pull", "", "read one exact module file through ADB sync; no module write")
	pullOutput := flag.String("pull-output", "", "new local output path for --pull; existing files are refused")
	debugPull := flag.Bool("debug-pull", false, "print ADB sync packet framing while using --pull")
	resetUSB := flag.Bool("reset-usb", false, "reset the current QDC507 USB session only; no configuration write")
	hilDial := flag.String("hil-dial", "", "one explicitly authorized real dial HIL target; requires --confirm-business-hil")
	hilHangup := flag.Bool("hil-hangup", false, "send one explicit real call_end HIL; requires --confirm-business-hil")
	hilSMSPDU := flag.String("hil-sms-pdu", "", "one explicitly authorized real GSM SMS PDU HIL; requires --confirm-business-hil")
	confirmBusinessHIL := flag.Bool("confirm-business-hil", false, "confirm that the selected business HIL may affect a real call")
	shellCmd := flag.String("shell", "", "run one arbitrary shell command on module (debug)")
	flag.Parse()
	if strings.TrimSpace(*output) == "" {
		fmt.Fprintln(os.Stderr, "--output is required")
		os.Exit(2)
	}
	if strings.TrimSpace(*shellCmd) != "" {
		if *probeInternalAT || *probeQMI || *probeQMILite || *probeM1 || *probeM2 || *persistenceStage != "" || *pullSource != "" || *resetUSB || strings.TrimSpace(*hilDial) != "" || strings.TrimSpace(*hilSMSPDU) != "" || *hilHangup {
			fmt.Fprintln(os.Stderr, "--shell cannot be combined with another probe or HIL")
			os.Exit(2)
		}
		result := runShell(strings.TrimSpace(*shellCmd))
		writeEvidence(*output, result)
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status != "SHELL_CAPTURED" {
			os.Exit(2)
		}
		return
	}

	if (*pullSource == "") != (*pullOutput == "") {
		fmt.Fprintln(os.Stderr, "--pull and --pull-output must be supplied together")
		os.Exit(2)
	}
	if strings.TrimSpace(*hilDial) != "" {
		if !*confirmBusinessHIL {
			fmt.Fprintln(os.Stderr, "--hil-dial requires --confirm-business-hil")
			os.Exit(2)
		}
		if !phoneNumberPattern.MatchString(strings.TrimSpace(*hilDial)) {
			fmt.Fprintln(os.Stderr, "--hil-dial accepts digits only, between 3 and 20 digits")
			os.Exit(2)
		}
		if *probeInternalAT || *probeQMI || *probeQMILite || *probeM1 || *probeM2 || *persistenceStage != "" || *pullSource != "" || *resetUSB {
			fmt.Fprintln(os.Stderr, "--hil-dial cannot be combined with another probe, --pull, --reset-usb, or persistence stage")
			os.Exit(2)
		}
		result := runDialHIL(strings.TrimSpace(*hilDial))
		writeEvidence(*output, result)
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status != "BUSINESS_HIL_CAPTURED" {
			os.Exit(2)
		}
		return
	}
	if strings.TrimSpace(*hilSMSPDU) != "" {
		if !*confirmBusinessHIL {
			fmt.Fprintln(os.Stderr, "--hil-sms-pdu requires --confirm-business-hil")
			os.Exit(2)
		}
		pdu := strings.TrimSpace(*hilSMSPDU)
		if !smsPDUPattern.MatchString(pdu) {
			fmt.Fprintln(os.Stderr, "--hil-sms-pdu accepts an even-length hexadecimal PDU")
			os.Exit(2)
		}
		if *probeInternalAT || *probeQMI || *probeQMILite || *probeM1 || *probeM2 || *persistenceStage != "" || *pullSource != "" || *resetUSB {
			fmt.Fprintln(os.Stderr, "--hil-sms-pdu cannot be combined with another probe, --pull, --reset-usb, or persistence stage")
			os.Exit(2)
		}
		result := runSMSHIL(pdu)
		writeEvidence(*output, result)
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status != "BUSINESS_HIL_CAPTURED" {
			os.Exit(2)
		}
		return
	}
	if *hilHangup {
		if !*confirmBusinessHIL {
			fmt.Fprintln(os.Stderr, "--hil-hangup requires --confirm-business-hil")
			os.Exit(2)
		}
		if *probeInternalAT || *probeQMI || *probeQMILite || *probeM1 || *probeM2 || *persistenceStage != "" || *pullSource != "" || *resetUSB {
			fmt.Fprintln(os.Stderr, "--hil-hangup cannot be combined with another probe, --pull, --reset-usb, or persistence stage")
			os.Exit(2)
		}
		result := runHangupHIL()
		writeEvidence(*output, result)
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status != "BUSINESS_HIL_CAPTURED" {
			os.Exit(2)
		}
		return
	}
	if *probeQMILite && (*probeQMI || *probeM1 || *probeInternalAT || *pullSource != "" || *resetUSB) {
		fmt.Fprintln(os.Stderr, "--probe-qmi-lite cannot be combined with another probe, --pull, or --reset-usb")
		os.Exit(2)
	}
	if *probeM1 && (*probeQMI || *probeQMILite || *probeInternalAT || *pullSource != "" || *resetUSB) {
		fmt.Fprintln(os.Stderr, "--probe-m1 cannot be combined with another probe, --pull, or --reset-usb")
		os.Exit(2)
	}
	if *probeM2 && (*probeQMI || *probeQMILite || *probeM1 || *probeInternalAT || *pullSource != "" || *resetUSB) {
		fmt.Fprintln(os.Stderr, "--probe-m2 cannot be combined with another probe, --pull, or --reset-usb")
		os.Exit(2)
	}
	if *persistenceStage != "" {
		if *probeInternalAT || *probeQMI || *probeQMILite || *probeM1 || *pullSource != "" || *resetUSB {
			fmt.Fprintln(os.Stderr, "--persistence-stage cannot be combined with another probe, --pull, or --reset-usb")
			os.Exit(2)
		}
		result := runPersistenceStage(*persistenceStage, *persistenceToken)
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode evidence: %v\n", err)
			os.Exit(2)
		}
		if err := os.WriteFile(*output, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write evidence: %v\n", err)
			os.Exit(2)
		}
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status == "UNKNOWN" || strings.HasPrefix(result.Status, "BLOCKED_") {
			os.Exit(2)
		}
		return
	}
	if *resetUSB {
		if *pullSource != "" || *probeInternalAT || *probeQMI {
			fmt.Fprintln(os.Stderr, "--reset-usb cannot be combined with probes or --pull")
			os.Exit(2)
		}
		result := resetUSBSession()
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "encode evidence: %v\n", err)
			os.Exit(2)
		}
		if err := os.WriteFile(*output, append(data, '\n'), 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write evidence: %v\n", err)
			os.Exit(2)
		}
		status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
		fmt.Println(string(status))
		if result.Status != "USB_RESET_REQUESTED" {
			os.Exit(2)
		}
		return
	}
	result := probe(*probeInternalAT, *probeQMI, *probeQMILite, *probeM1, *probeM2, *pullSource, *pullOutput, *debugPull)
	writeEvidence(*output, result)
	status, _ := json.Marshal(map[string]string{"status": result.Status, "output": *output})
	fmt.Println(string(status))
	if result.Status != "H1_M0_READ_ONLY_CAPTURED" {
		os.Exit(2)
	}
}

func writeEvidence(output string, result evidence) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode evidence: %v\n", err)
		os.Exit(2)
	}
	if err := os.WriteFile(output, append(data, '\n'), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write evidence: %v\n", err)
		os.Exit(2)
	}
}

func resetUSBSession() evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-adb-readonly.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   true,
		VendorID:   fmt.Sprintf("0x%04x", vendorID),
		ProductID:  fmt.Sprintf("0x%04x", productID),
		Status:     "UNKNOWN",
	}
	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	defer C.libusb_exit(context)
	handle := C.libusb_open_device_with_vid_pid(context, C.uint16_t(vendorID), C.uint16_t(productID))
	if handle == nil {
		result.Status = "BLOCKED_QDC507_NOT_FOUND"
		result.Error = "QDC507 USB device 2c7c:0125 was not found by libusb"
		return result
	}
	defer C.libusb_close(handle)
	if rc := C.libusb_reset_device(handle); rc != 0 {
		result.Status = "BLOCKED_USB_RESET"
		result.Error = usbError(rc)
		return result
	}
	result.Status = "USB_RESET_REQUESTED"
	return result
}

func runDialHIL(number string) evidence {
	return runBusinessCommandHIL(
		"cellbridge.v8.qdc507-business-hil.v1",
		"dial_state_hangup",
		dialHILCommand(number),
	)
}

func runHangupHIL() evidence {
	return runBusinessCommandHIL(
		"cellbridge.v8.qdc507-business-hil.v1",
		"hangup_state",
		"{ sleep 5; printf 'call_end\\n'; sleep 3; printf 'call_state\\n'; sleep 1; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 20 -s KILL /usr/bin/qmi_simple_ril_test 2>&1",
	)
}

func runSMSHIL(pdu string) evidence {
	return runBusinessCommandHIL(
		"cellbridge.v8.qdc507-sms-hil.v1",
		"sms_submit_pdu",
		fmt.Sprintf("{ sleep 5; printf 'mo_sms_gsm %s\\n'; sleep 12; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 25 -s KILL /usr/bin/qmi_simple_ril_test 2>&1", pdu),
	)
}

func runShell(command string) evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-shell.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   true,
		VendorID:   fmt.Sprintf("0x%04x", vendorID),
		ProductID:  fmt.Sprintf("0x%04x", productID),
		Status:     "UNKNOWN",
	}
	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	device, err := openADBDevice(context)
	if err != nil {
		C.libusb_exit(context)
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	selected := device.public()
	result.ADBInterface = &selected
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	result.Handshake = "CNXN"
	identity, err := device.shell("id")
	if err != nil {
		result.Status = "BLOCKED_ADB_SHELL"
		result.Error = err.Error()
		return result
	}
	result.ShellIdentity = strings.TrimSpace(identity)
	result.ShellIsRoot = strings.Contains(identity, "uid=0")
	output, err := device.shell(command)
	if err != nil {
		result.Status = "SHELL_FAILED"
		result.Error = err.Error()
		return result
	}
	result.Status = "SHELL_CAPTURED"
	result.Commands = []shellResult{{Name: "shell", Command: command, ReturnCode: 0, Stdout: strings.TrimSpace(output)}}
	return result
}

func runBusinessCommandHIL(schema, name, command string) evidence {
	result := evidence{
		Schema:     schema,
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   false,
		VendorID:   fmt.Sprintf("0x%04x", vendorID),
		ProductID:  fmt.Sprintf("0x%04x", productID),
		Status:     "UNKNOWN",
	}
	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	device, err := openADBDevice(context)
	if err != nil {
		C.libusb_exit(context)
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	selected := device.public()
	result.ADBInterface = &selected
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	result.Handshake = "CNXN"
	identity, err := device.shell("id")
	if err != nil {
		result.Status = "BLOCKED_ADB_SHELL"
		result.Error = err.Error()
		return result
	}
	result.ShellIdentity = trim(identity)
	result.ShellIsRoot = strings.Contains(identity, "uid=0")
	if !result.ShellIsRoot {
		result.Status = "BLOCKED_NON_ROOT_SHELL"
		result.Error = "business HIL requires the already-authorized root shell; no privilege escalation was attempted"
		return result
	}
	output, err := device.shell(command)
	entry := shellResult{Name: name, Command: command, ReturnCode: 0, Stdout: trim(output)}
	if err != nil {
		entry.ReturnCode = -1
		entry.Error = err.Error()
		result.Status = "BLOCKED_BUSINESS_HIL"
		result.Error = err.Error()
	} else {
		result.Status = "BUSINESS_HIL_CAPTURED"
	}
	result.Commands = []shellResult{entry}
	return result
}

func dialHILCommand(number string) string {
	return fmt.Sprintf("{ sleep 5; printf 'dial %s\\n'; sleep 25; printf 'call_state\\n'; sleep 3; printf 'call_end\\n'; sleep 3; printf 'call_state\\n'; sleep 1; printf 'quit\\n'; } 2>&1 | /bin/timeout -t 45 -s KILL /usr/bin/qmi_simple_ril_test 2>&1", number)
}

func probe(probeInternalAT, probeQMI, probeQMILite, probeM1, probeM2 bool, pullSource, pullOutput string, debugPull bool) evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-adb-readonly.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   true,
		VendorID:   fmt.Sprintf("0x%04x", vendorID),
		ProductID:  fmt.Sprintf("0x%04x", productID),
		Status:     "UNKNOWN",
	}

	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	device, err := openADBDevice(context)
	if err != nil {
		C.libusb_exit(context)
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()

	selected := device.public()
	result.ADBInterface = &selected

	if err := device.handshake(); err != nil {
		if errors.Is(err, errADBAuthRequired) {
			result.Status = "BLOCKED_ADB_AUTH_REQUIRED"
		} else {
			result.Status = "BLOCKED_ADB_HANDSHAKE"
		}
		result.Error = err.Error()
		return result
	}
	result.Handshake = "CNXN"
	if pullSource != "" {
		if err := device.pull(pullSource, pullOutput, debugPull); err != nil {
			result.Status = "BLOCKED_ADB_READ"
			result.Error = err.Error()
			return result
		}
		result.PulledFile = pullOutput
	}

	identity, err := device.shell("id")
	if err != nil {
		result.Status = "BLOCKED_ADB_SHELL"
		result.Error = err.Error()
		return result
	}
	result.ShellIdentity = trim(identity)
	result.ShellIsRoot = strings.Contains(identity, "uid=0")
	if !result.ShellIsRoot {
		result.Status = "BLOCKED_NON_ROOT_SHELL"
		result.Error = "v8 M0 requires a verified root shell; no privilege escalation was attempted"
		return result
	}

	commands := readCommands
	if probeQMI {
		commands = append(commands, qmiReadCommands...)
	}
	for _, item := range commands {
		output, err := device.shell(item.Command)
		entry := shellResult{Name: item.Name, Command: item.Command, ReturnCode: 0, Stdout: trim(output)}
		if err != nil {
			entry.ReturnCode = -1
			entry.Error = err.Error()
		}
		result.Commands = append(result.Commands, entry)
	}
	if probeQMILite {
		output, err := device.shell(qmiReadOnlyConsole)
		entry := shellResult{Name: "qmi_readonly_console_lite", Command: qmiReadOnlyConsole, ReturnCode: 0, Stdout: trim(output)}
		if err != nil {
			entry.ReturnCode = -1
			entry.Error = err.Error()
		}
		result.Commands = append(result.Commands, entry)
	}
	if probeM1 {
		for _, item := range m1ReadCommands {
			output, err := device.shell(item.Command)
			entry := shellResult{Name: item.Name, Command: item.Command, ReturnCode: 0, Stdout: trim(output)}
			if err != nil {
				entry.ReturnCode = -1
				entry.Error = err.Error()
			}
			result.Commands = append(result.Commands, entry)
		}
	}
	if probeM2 {
		for _, item := range m2ReadCommands {
			output, err := device.shell(item.Command)
			entry := shellResult{Name: item.Name, Command: item.Command, ReturnCode: 0, Stdout: trim(output)}
			if err != nil {
				entry.ReturnCode = -1
				entry.Error = err.Error()
			}
			result.Commands = append(result.Commands, entry)
		}
	}
	if probeInternalAT {
		for _, devicePath := range []string{"/dev/at_usb0", "/dev/at_usb1"} {
			for _, query := range []string{"AT", "ATI", "AT+CPIN?", "AT+COPS?", "AT+CSQ", "AT+CLCC"} {
				command := internalATProbeCommand(devicePath, query)
				output, err := device.shell(command)
				entry := shellResult{Name: "internal_at_" + strings.TrimPrefix(devicePath, "/dev/") + "_" + strings.ReplaceAll(query, "+", "plus"), Command: command, ReturnCode: 0, Stdout: trim(output)}
				if err != nil {
					entry.ReturnCode = -1
					entry.Error = err.Error()
				}
				result.Commands = append(result.Commands, entry)
			}
		}
	}
	result.Status = "H1_M0_READ_ONLY_CAPTURED"
	return result
}

func runPersistenceStage(stage, token string) evidence {
	result := evidence{
		Schema:     "cellbridge.v8.qdc507-adb-persistence.v1",
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ReadOnly:   false,
		VendorID:   fmt.Sprintf("0x%04x", vendorID),
		ProductID:  fmt.Sprintf("0x%04x", productID),
		Status:     "UNKNOWN",
		PulledFile: persistenceStageDir,
	}
	if stage != "create" && stage != "verify" && stage != "verify-reboot" && stage != "cleanup" {
		result.Status = "BLOCKED_INVALID_PERSISTENCE_STAGE"
		result.Error = "stage must be create, verify, verify-reboot, or cleanup"
		return result
	}
	if stage == "create" {
		var err error
		token, err = newPersistenceToken()
		if err != nil {
			result.Status = "BLOCKED_TOKEN_GENERATION"
			result.Error = err.Error()
			return result
		}
		result.PulledFile = persistenceStageDir
		result.ProbeToken = token
	}
	if stage != "create" && stage != "cleanup" && strings.TrimSpace(token) == "" {
		result.Status = "BLOCKED_PERSISTENCE_TOKEN_REQUIRED"
		result.Error = "--persistence-token is required for verify stages"
		return result
	}
	result.ShellIdentity = ""
	var context *C.libusb_context
	if rc := C.libusb_init(&context); rc != 0 {
		result.Status = "BLOCKED_LIBUSB_INIT"
		result.Error = usbError(rc)
		return result
	}
	device, err := openADBDevice(context)
	if err != nil {
		C.libusb_exit(context)
		result.Status = statusForOpenError(err)
		result.Error = err.Error()
		return result
	}
	defer device.close()
	selected := device.public()
	result.ADBInterface = &selected
	if err := device.handshake(); err != nil {
		result.Status = "BLOCKED_ADB_HANDSHAKE"
		result.Error = err.Error()
		return result
	}
	result.Handshake = "CNXN"
	identity, err := device.shell("id")
	if err != nil {
		result.Status = "BLOCKED_ADB_SHELL"
		result.Error = err.Error()
		return result
	}
	result.ShellIdentity = trim(identity)
	result.ShellIsRoot = strings.Contains(identity, "uid=0")
	if !result.ShellIsRoot {
		result.Status = "BLOCKED_NON_ROOT_SHELL"
		result.Error = "M1 persistence probe requires the already verified root shell"
		return result
	}

	var command string
	switch stage {
	case "create":
		command = fmt.Sprintf("umask 077; mkdir -p %s && printf '%%s\\n' %s > %s && cp /bin/busybox %s && chmod 700 %s && %s true; rc=$?; echo EXEC_RC=$rc; sync", persistenceStageDir, shellQuote(token), persistenceStageMarker, persistenceStageBinary, persistenceStageBinary, persistenceStageBinary)
	case "verify", "verify-reboot":
		command = fmt.Sprintf("marker=$(cat %s 2>/dev/null) || exit 11; printf 'MARKER=%%s\\n' \"$marker\"; [ \"$marker\" = %s ] || exit 12; %s; rc=$?; echo EXEC_RC=$rc; mount | grep ' /data '", persistenceStageMarker, shellQuote(token), persistenceStageBinary)
	case "cleanup":
		command = fmt.Sprintf("rm -f %s %s; rmdir %s 2>/dev/null || true; if [ -e %s ] || [ -e %s ]; then echo CLEANUP_REMAINING=1; else echo CLEANUP_REMAINING=0; fi", persistenceStageMarker, persistenceStageBinary, persistenceStageDir, persistenceStageMarker, persistenceStageBinary)
	}
	output, err := device.shell(command)
	entry := shellResult{Name: "persistence_" + stage, Command: command, ReturnCode: 0, Stdout: trim(output)}
	if err != nil {
		entry.ReturnCode = -1
		entry.Error = err.Error()
	}
	result.Commands = append(result.Commands, entry)
	if stage == "create" {
		result.ShellIdentity = trim(result.ShellIdentity)
		result.Error = "persistence token returned in probeToken"
		result.Status = "M1_PERSISTENCE_PROBE_CREATED"
		if !strings.Contains(output, "EXEC_RC=0") {
			result.Status = "BLOCKED_PERSISTENCE_EXEC"
			result.Error = "persistence probe binary did not execute successfully"
			return result
		}
		if rebootErr := requestModuleReboot(device, &result); rebootErr != nil {
			result.Status = "BLOCKED_MODULE_REBOOT"
			result.Error = rebootErr.Error()
			return result
		}
		result.Status = "M1_PERSISTENCE_PROBE_REBOOT_REQUESTED"
		result.Error = "persistence token returned in probeToken"
		return result
	}
	if stage == "verify" || stage == "verify-reboot" {
		if !strings.Contains(output, "MARKER="+token) || !strings.Contains(output, "EXEC_RC=0") {
			result.Status = "BLOCKED_PERSISTENCE_VERIFY"
			result.Error = "marker or executable did not survive the reboot"
			return result
		}
		result.Status = "M1_PERSISTENCE_VERIFIED"
		if stage == "verify-reboot" {
			if rebootErr := requestModuleReboot(device, &result); rebootErr != nil {
				result.Status = "BLOCKED_MODULE_REBOOT"
				result.Error = rebootErr.Error()
				return result
			}
			result.Status = "M1_PERSISTENCE_VERIFIED_REBOOT_REQUESTED"
		}
		return result
	}
	if !strings.Contains(output, "CLEANUP_REMAINING=0") {
		result.Status = "BLOCKED_PERSISTENCE_CLEANUP"
		result.Error = "probe files were not fully removed"
		return result
	}
	result.Status = "M1_PERSISTENCE_CLEANED"
	return result
}

func newPersistenceToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate persistence token: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func requestModuleReboot(device *usbADB, result *evidence) error {
	output, err := device.shell("sync; reboot")
	entry := shellResult{Name: "persistence_reboot", Command: "sync; reboot", ReturnCode: 0, Stdout: trim(output)}
	if err != nil {
		entry.ReturnCode = -1
		entry.Error = err.Error()
		// A reboot commonly closes the ADB shell before it can return CLSE. It is
		// still considered requested when the transport has already disappeared.
		if !strings.Contains(strings.ToLower(err.Error()), "timeout") && !strings.Contains(strings.ToLower(err.Error()), "no device") && !strings.Contains(strings.ToLower(err.Error()), "transfer") {
			result.Commands = append(result.Commands, entry)
			return err
		}
	}
	result.Commands = append(result.Commands, entry)
	return nil
}

func internalATProbeCommand(device, query string) string {
	inner := fmt.Sprintf("exec 3<>%s || exit 1; echo OPENED; printf '%%s\\r' %s >&3; echo SENT; /bin/cat <&3", device, shellQuote(query))
	return fmt.Sprintf("if [ ! -c %s ]; then echo 'MISSING'; else /bin/timeout -t 2 /bin/sh -c %s; fi", device, shellQuote(inner))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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

func (d *usbADB) public() adbInterface {
	return adbInterface{
		Interface:   d.iface,
		Class:       "0xff",
		Subclass:    "0x42",
		Protocol:    "0x01",
		EndpointIn:  fmt.Sprintf("0x%02x", d.in),
		EndpointOut: fmt.Sprintf("0x%02x", d.out),
	}
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

var errADBAuthRequired = errors.New("ADB device requested AUTH; no key or authorization was attempted")

func (d *usbADB) shell(command string) (string, error) {
	const localID = 1
	if err := d.writePacket(packet{Command: adbOPEN, Arg0: localID, Payload: append([]byte("shell:"+command), 0)}); err != nil {
		return "", fmt.Errorf("open shell %q: %w", command, err)
	}

	var remoteID uint32
	var output strings.Builder
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return output.String(), fmt.Errorf("read shell %q: %w", command, err)
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
				return output.String(), fmt.Errorf("ack shell %q: %w", command, err)
			}
		case adbCLSE:
			_ = d.writePacket(packet{Command: adbCLSE, Arg0: localID, Arg1: response.Arg0})
			return output.String(), nil
		default:
			return output.String(), fmt.Errorf("unexpected shell command 0x%08x", response.Command)
		}
	}
	return output.String(), fmt.Errorf("shell %q timed out", command)
}

// pull reads one explicitly named module file through ADB sync. It is kept
// separate from shell so the probe cannot accidentally turn a path into a
// shell command. The local destination must not already exist.
func (d *usbADB) pull(remotePath, localPath string, debug bool) error {
	if !strings.HasPrefix(remotePath, "/") || strings.ContainsRune(remotePath, 0) {
		return errors.New("ADB pull path must be an absolute module path without NUL")
	}
	if localPath == "" {
		return errors.New("ADB pull output path is required")
	}
	if _, err := os.Stat(localPath); err == nil {
		return fmt.Errorf("refusing to overwrite existing local file %s", localPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check local pull output: %w", err)
	}

	localID := uint32(2)
	remoteID, err := d.openService(localID, "sync:")
	if err != nil {
		return err
	}
	defer func() { _ = d.writePacket(packet{Command: adbCLSE, Arg0: localID, Arg1: remoteID}) }()

	path := []byte(remotePath)
	request := make([]byte, 8+len(path))
	binary.LittleEndian.PutUint32(request[0:4], syncRECV)
	binary.LittleEndian.PutUint32(request[4:8], uint32(len(path)))
	copy(request[8:], path)
	if err := d.writePacket(packet{Command: adbWRTE, Arg0: localID, Arg1: remoteID, Payload: request}); err != nil {
		return fmt.Errorf("send ADB RECV: %w", err)
	}

	directory := filepath.Dir(localPath)
	temporary, err := os.CreateTemp(directory, ".cellbridge-adb-pull-*")
	if err != nil {
		return fmt.Errorf("create local pull temporary: %w", err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()

	var total int64
	var syncBuffer []byte
	for {
		response, err := d.readPacket(adbTimeout)
		if err != nil {
			return fmt.Errorf("read ADB sync response: %w", err)
		}
		if debug {
			preview := response.Payload
			if len(preview) > 16 {
				preview = preview[:16]
			}
			fmt.Fprintf(os.Stderr, "adb-sync packet cmd=0x%08x payload=%d preview=%x\n", response.Command, len(response.Payload), preview)
		}
		switch response.Command {
		case adbOKAY:
			// The sync service acknowledges each host WRTE before it sends
			// the next DATA/DONE record.
			remoteID = response.Arg0
		case adbWRTE:
			if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: remoteID}); err != nil {
				return fmt.Errorf("ack ADB sync response: %w", err)
			}
			syncBuffer = append(syncBuffer, response.Payload...)
			for {
				if len(syncBuffer) < 8 {
					break
				}
				kind := binary.LittleEndian.Uint32(syncBuffer[0:4])
				length := binary.LittleEndian.Uint32(syncBuffer[4:8])
				if length > packetLimit {
					return fmt.Errorf("invalid ADB sync record length kind=0x%08x length=%d buffered=%d", kind, length, len(syncBuffer))
				}
				recordSize := 8 + int(length)
				if len(syncBuffer) < recordSize {
					break
				}
				data := syncBuffer[8:recordSize]
				syncBuffer = syncBuffer[recordSize:]
				if debug {
					fmt.Fprintf(os.Stderr, "adb-sync record kind=0x%08x length=%d buffered=%d\n", kind, length, len(syncBuffer))
				}
				switch kind {
				case syncDATA:
					total += int64(len(data))
					if total > 16*1024*1024 {
						return errors.New("ADB pull exceeds 16 MiB safety limit")
					}
					if _, err := temporary.Write(data); err != nil {
						return fmt.Errorf("write pulled data: %w", err)
					}
				case syncDONE:
					if length != 0 {
						return errors.New("ADB sync DONE record has payload")
					}
					if err := temporary.Close(); err != nil {
						return fmt.Errorf("close pulled data: %w", err)
					}
					if err := os.Rename(temporaryName, localPath); err != nil {
						return fmt.Errorf("publish pulled file: %w", err)
					}
					removeTemporary = false
					// End the sync stream before closing libusb. This avoids leaving a
					// stale CLSE/OKAY packet for the next direct-USB probe.
					_ = d.closeService(localID, remoteID)
					return nil
				case syncFAIL:
					return fmt.Errorf("module rejected ADB pull: %s", string(data))
				default:
					return fmt.Errorf("unknown ADB sync record 0x%08x", kind)
				}
			}
		case adbCLSE:
			return errors.New("module closed ADB sync before DONE")
		default:
			return fmt.Errorf("unexpected ADB sync packet 0x%08x", response.Command)
		}
	}
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
			if err := d.writePacket(packet{Command: adbOKAY, Arg0: localID, Arg1: remoteID}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected ADB close packet 0x%08x", response.Command)
		}
	}
	return errors.New("ADB service close timed out")
}

func (d *usbADB) openService(localID uint32, service string) (uint32, error) {
	if err := d.writePacket(packet{Command: adbOPEN, Arg0: localID, Payload: append([]byte(service), 0)}); err != nil {
		return 0, fmt.Errorf("open ADB %s: %w", service, err)
	}
	deadline := time.Now().Add(adbTimeout)
	for time.Now().Before(deadline) {
		response, err := d.readPacket(time.Until(deadline))
		if err != nil {
			return 0, fmt.Errorf("read ADB %s open: %w", service, err)
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
		if err := d.bulkWrite(message.Payload); err != nil {
			return err
		}
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
	message := packet{
		Command: binary.LittleEndian.Uint32(header[0:4]),
		Arg0:    binary.LittleEndian.Uint32(header[4:8]),
		Arg1:    binary.LittleEndian.Uint32(header[8:12]),
	}
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

func statusForOpenError(err error) string {
	if strings.Contains(err.Error(), "no ff/42/01") {
		return "BLOCKED_NO_ADB_INTERFACE"
	}
	if strings.Contains(err.Error(), "not found") {
		return "BLOCKED_QDC507_NOT_FOUND"
	}
	return "BLOCKED_ADB_OPEN"
}
