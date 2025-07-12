package config

// TransportType defines the type of transport.
type TransportType string

const (
	TCP    TransportType = "tcp"
	TCPMUX TransportType = "tcpmux"
	WS     TransportType = "ws"
	WSS    TransportType = "wss"
	WSMUX  TransportType = "wsmux"
	WSSMUX TransportType = "wssmux"
	QUIC   TransportType = "quic"
	UDP    TransportType = "udp"
)

// ServerConfig represents the configuration for the server.
type ServerConfig struct {
	BindAddr         string        `toml:"bind_addr"`
	Transport        TransportType `toml:"transport"`
	Token            string        `toml:"token"`
	Nodelay          bool          `toml:"nodelay"`
	Keepalive        int           `toml:"keepalive_period"` // seconds
	ChannelSize      int           `toml:"channel_size"`
	LogLevel         string        `toml:"log_level"`
	Ports            []string      `toml:"ports"`
	PPROF            bool          `toml:"pprof"`
	MuxSession       int           `toml:"mux_session"`
	MuxVersion       int           `toml:"mux_version"`
	MaxFrameSize     int           `toml:"mux_framesize"`
	MaxReceiveBuffer int           `toml:"mux_receivebuffer"` // fixed typo
	MaxStreamBuffer  int           `toml:"mux_streambuffer"`
	Sniffer          bool          `toml:"sniffer"`
	WebPort          int           `toml:"web_port"`
	SnifferLog       string        `toml:"sniffer_log"`
	TLSCertFile      string        `toml:"tls_cert"`
	TLSKeyFile       string        `toml:"tls_key"`
	Heartbeat        int           `toml:"heartbeat"` // seconds
	MuxCon           int           `toml:"mux_con"`
	AcceptUDP        bool          `toml:"accept_udp"`
}

// ClientConfig represents the configuration for the client.
type ClientConfig struct {
	RemoteAddr       string        `toml:"remote_addr"`
	Transport        TransportType `toml:"transport"`
	Token            string        `toml:"token"`
	ConnectionPool   int           `toml:"connection_pool"`
	RetryInterval    int           `toml:"retry_interval"`    // seconds
	Nodelay          bool          `toml:"nodelay"`
	Keepalive        int           `toml:"keepalive_period"`  // seconds
	LogLevel         string        `toml:"log_level"`
	PPROF            bool          `toml:"pprof"`
	MuxSession       int           `toml:"mux_session"`
	MuxVersion       int           `toml:"mux_version"`
	MaxFrameSize     int           `toml:"mux_framesize"`
	MaxReceiveBuffer int           `toml:"mux_receivebuffer"` // fixed typo
	MaxStreamBuffer  int           `toml:"mux_streambuffer"`
	Sniffer          bool          `toml:"sniffer"`
	WebPort          int           `toml:"web_port"`
	SnifferLog       string        `toml:"sniffer_log"`
	DialTimeout      int           `toml:"dial_timeout"`      // seconds
	AggressivePool   bool          `toml:"aggressive_pool"`
	EdgeIP           string        `toml:"edge_ip"`
}

// Config represents the complete configuration, including both server and client settings.
type Config struct {
	Server ServerConfig `toml:"server"`
	Client ClientConfig `toml:"client"`
}

// ApplyDefaults applies sane default values if not set (call this after parsing)
func (c *Config) ApplyDefaults() {
	// Server defaults
	if c.Server.Keepalive <= 0 {
		c.Server.Keepalive = 60 // default 60 seconds keepalive
	}
	if c.Server.MuxVersion <= 0 {
		c.Server.MuxVersion = 1
	}
	if c.Server.MaxFrameSize <= 0 {
		c.Server.MaxFrameSize = 64 * 1024
	}
	if c.Server.MaxReceiveBuffer <= 0 {
		c.Server.MaxReceiveBuffer = 1024 * 1024
	}
	if c.Server.MaxStreamBuffer <= 0 {
		c.Server.MaxStreamBuffer = 128 * 1024
	}

	// Client defaults
	if c.Client.Keepalive <= 0 {
		c.Client.Keepalive = 60
	}
	if c.Client.RetryInterval <= 0 {
		c.Client.RetryInterval = 5
	}
	if c.Client.DialTimeout <= 0 {
		c.Client.DialTimeout = 10
	}
	if c.Client.MuxVersion <= 0 {
		c.Client.MuxVersion = 1
	}
	if c.Client.MaxFrameSize <= 0 {
		c.Client.MaxFrameSize = 64 * 1024
	}
	if c.Client.MaxReceiveBuffer <= 0 {
		c.Client.MaxReceiveBuffer = 1024 * 1024
	}
	if c.Client.MaxStreamBuffer <= 0 {
		c.Client.MaxStreamBuffer = 128 * 1024
	}
	if c.Client.ConnectionPool <= 0 {
		c.Client.ConnectionPool = 4
	}
}
