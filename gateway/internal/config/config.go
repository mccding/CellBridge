package config

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Network struct {
		Mode            string `yaml:"mode"`
		Transport       string `yaml:"transport"`
		TailnetHostname string `yaml:"tailnet_hostname"`
		PublicFallback  bool   `yaml:"public_fallback"`
	} `yaml:"network"`
	Server struct {
		Listen        string `yaml:"listen"`
		PublicBaseURL string `yaml:"public_base_url"`
	} `yaml:"server"`
	Data struct {
		Dir string `yaml:"dir"`
	} `yaml:"data"`
	Modem struct {
		Adapter                 string `yaml:"adapter"`
		TTY                     string `yaml:"tty"`
		Baud                    int    `yaml:"baud"`
		AllowUSBIdentityChanges bool   `yaml:"allow_usb_identity_changes"`
	} `yaml:"modem"`
	Voice struct {
		Enabled    bool   `yaml:"enabled"`
		Backend    string `yaml:"backend"`
		Codec      string `yaml:"codec"`
		SampleRate int    `yaml:"sample_rate"`
		RXPath     string `yaml:"rx_path"`
		TXPath     string `yaml:"tx_path"`
		RuntimeDir string `yaml:"runtime_dir"`
		Bootstrap  bool   `yaml:"bootstrap"`
	} `yaml:"voice"`
	WebRTC struct {
		RemoteICEPolicy    string `yaml:"remote_ice_policy"`
		LocalICEPolicy     string `yaml:"local_ice_policy"`
		MediaNetworkPolicy string `yaml:"media_network_policy"`
		TurnCredentialTTL  string `yaml:"turn_credential_ttl"`
		PrivateTURN        struct {
			Enabled bool   `yaml:"enabled"`
			Host    string `yaml:"host"`
			Port    int    `yaml:"port"`
			BindIP  string `yaml:"bind_ip"`
		} `yaml:"private_turn"`
	} `yaml:"webrtc"`
	Push struct {
		Mode      string `yaml:"mode"`
		BrokerURL string `yaml:"broker_url"`
	} `yaml:"push"`
	Recording struct {
		Enabled          bool   `yaml:"enabled"`
		RetentionDays    int    `yaml:"retention_days"`
		MinimumFreeBytes int64  `yaml:"minimum_free_bytes"`
		AutoMode         string `yaml:"auto_mode"`
	} `yaml:"recording"`
	Security struct {
		PairingLocalOnly bool `yaml:"pairing_local_only"`
		AdminLocalOnly   bool `yaml:"admin_local_only"`
		RedactLogs       bool `yaml:"redact_logs"`
	} `yaml:"security"`
	SIP struct {
		Enabled   bool   `yaml:"enabled"`
		Listen    string `yaml:"listen"`
		Realm     string `yaml:"realm"`
		PushToken string `yaml:"push_token"`
		Users     []struct {
			Username string `yaml:"username"`
			Password string `yaml:"password"`
		} `yaml:"users"`
	} `yaml:"sip"`
}

func Default() Config {
	var result Config
	result.Network.Mode = "tailnet"
	result.Network.Transport = "tailnet"
	result.Network.PublicFallback = false
	result.Server.Listen = "127.0.0.1:8787"
	result.Data.Dir = "/var/lib/cellbridge"
	result.Modem.Adapter = "auto"
	result.Modem.TTY = "auto"
	result.Modem.Baud = 9600
	result.Voice.Enabled = true
	result.Voice.Backend = "auto"
	result.Voice.Codec = "pcmu"
	result.Voice.SampleRate = 8000
	result.WebRTC.RemoteICEPolicy = "relay"
	result.WebRTC.LocalICEPolicy = "all"
	result.WebRTC.MediaNetworkPolicy = "tailnet-turn"
	result.WebRTC.TurnCredentialTTL = "10m"
	result.WebRTC.PrivateTURN.Port = 3478
	result.Push.Mode = "broker"
	result.Recording.Enabled = true
	result.Recording.RetentionDays = 90
	result.Recording.MinimumFreeBytes = 500 * 1024 * 1024
	result.Recording.AutoMode = "off"
	result.Security.PairingLocalOnly = true
	result.Security.AdminLocalOnly = true
	result.Security.RedactLogs = true
	result.SIP.Enabled = false
	result.SIP.Listen = "127.0.0.1:5060"
	result.SIP.Realm = "cellbridge"
	return result
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	result := Default()
	if err := yaml.Unmarshal(data, &result); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := result.Validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (c Config) Validate() error {
	if c.Server.Listen == "" || c.Data.Dir == "" {
		return fmt.Errorf("server.listen and data.dir are required")
	}
	if c.Network.Mode != "tailnet" && c.Network.Mode != "pocket" {
		return fmt.Errorf("network.mode must be tailnet or pocket")
	}
	if c.Network.Mode == "tailnet" && c.Network.Transport != "" && c.Network.Transport != "tailnet" {
		return fmt.Errorf("tailnet mode requires network.transport=tailnet")
	}
	if c.Network.Mode == "pocket" && c.Network.Transport != "" && c.Network.Transport != "pocketUSB" && c.Network.Transport != "pocketWiFi" {
		return fmt.Errorf("pocket mode transport must be pocketUSB or pocketWiFi")
	}
	if c.Network.PublicFallback {
		return fmt.Errorf("network.public_fallback must remain false")
	}
	if c.Network.Mode == "tailnet" {
		if !isLoopbackAddress(c.Server.Listen) {
			return fmt.Errorf("tailnet mode requires server.listen to bind loopback")
		}
		if strings.TrimSpace(c.Server.PublicBaseURL) != "" {
			return fmt.Errorf("tailnet mode must not configure server.public_base_url")
		}
		if hostname := strings.TrimSpace(c.Network.TailnetHostname); hostname != "" {
			if strings.Contains(hostname, "://") || !strings.HasSuffix(strings.ToLower(hostname), ".ts.net") {
				return fmt.Errorf("network.tailnet_hostname must be an HTTPS *.ts.net hostname")
			}
		}
	}
	if c.Modem.Baud != 9600 && c.Modem.Baud != 115200 {
		return fmt.Errorf("modem.baud must be 9600 or 115200")
	}
	if c.Voice.Enabled && (c.Voice.Codec != "pcmu" || c.Voice.SampleRate != 8000) {
		return fmt.Errorf("V1 voice requires pcmu at 8000 Hz")
	}
	if c.Voice.Backend != "auto" && c.Voice.Backend != "raw-pcm" && c.Voice.Backend != "alsa" && c.Voice.Backend != "qdc507" {
		return fmt.Errorf("voice.backend must be auto, raw-pcm, alsa, or qdc507")
	}
	if c.Voice.Enabled && (c.Voice.Backend == "raw-pcm" || c.Voice.Backend == "alsa") && (c.Voice.RXPath == "" || c.Voice.TXPath == "") {
		return fmt.Errorf("%s voice backend requires voice.rx_path and voice.tx_path", c.Voice.Backend)
	}
	if c.Voice.Enabled && c.Voice.Backend == "qdc507" && (c.Voice.RXPath == "" || c.Voice.TXPath == "" || c.Voice.RuntimeDir == "") {
		return fmt.Errorf("qdc507 voice backend requires voice.rx_path, voice.tx_path, and voice.runtime_dir")
	}
	if c.Recording.RetentionDays < 0 {
		return fmt.Errorf("recording.retention_days cannot be negative")
	}
	if c.Recording.MinimumFreeBytes < 0 {
		return fmt.Errorf("recording.minimum_free_bytes cannot be negative")
	}
	if c.Recording.AutoMode != "off" && c.Recording.AutoMode != "all" && c.Recording.AutoMode != "incoming" && c.Recording.AutoMode != "outgoing" {
		return fmt.Errorf("recording.auto_mode must be off, all, incoming, or outgoing")
	}
	if c.WebRTC.RemoteICEPolicy != "relay" {
		return fmt.Errorf("remote_ice_policy must be relay")
	}
	if c.WebRTC.LocalICEPolicy != "all" && c.WebRTC.LocalICEPolicy != "relay" {
		return fmt.Errorf("local_ice_policy must be all or relay")
	}
	if c.WebRTC.MediaNetworkPolicy != "tailnet-turn" && c.WebRTC.MediaNetworkPolicy != "tailnet-direct" {
		return fmt.Errorf("media_network_policy must be tailnet-turn or tailnet-direct")
	}
	if c.WebRTC.PrivateTURN.Enabled {
		if strings.TrimSpace(c.WebRTC.PrivateTURN.Host) == "" || c.WebRTC.PrivateTURN.Port < 1 || c.WebRTC.PrivateTURN.Port > 65535 {
			return fmt.Errorf("enabled private TURN requires webrtc.private_turn.host and a valid port")
		}
		if c.Network.Mode == "tailnet" && strings.Contains(c.WebRTC.PrivateTURN.Host, "://") {
			return fmt.Errorf("private TURN host must not include a scheme")
		}
	}
	if c.Security.PairingLocalOnly == false {
		return fmt.Errorf("pairing_local_only cannot be disabled in V1")
	}
	return nil
}

func isLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
