package client

import (
	"context"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/musix/backhaul/internal/config"
	"github.com/musix/backhaul/internal/client/transport"
	"github.com/musix/backhaul/internal/utils"
	"github.com/sirupsen/logrus"
)

type Client struct {
	config *config.ClientConfig
	ctx    context.Context
	cancel context.CancelFunc
	logger *logrus.Logger
}

func NewClient(cfg *config.ClientConfig, parentCtx context.Context) *Client {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Client{
		config: cfg,
		ctx:    ctx,
		cancel: cancel,
		logger: utils.NewLogger(cfg.LogLevel),
	}
}

func (c *Client) Start() {
	if c.config.PPROF {
		go func() {
			c.logger.Info("pprof started at port 6061")
			if err := http.ListenAndServe("0.0.0.0:6061", nil); err != nil {
				c.logger.Errorf("pprof server error: %v", err)
			}
		}()
	}

	c.logger.Infof("client with remote address %s started successfully", c.config.RemoteAddr)

	// Launch the appropriate transport client based on config.Transport
	switch c.config.Transport {
	case config.TCP:
		tcpConfig := &transport.TcpConfig{
			RemoteAddr:     c.config.RemoteAddr,
			Nodelay:        c.config.Nodelay,
			KeepAlive:      time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval:  time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:    time.Duration(c.config.DialTimeout) * time.Second,
			ConnPoolSize:   c.config.ConnectionPool,
			Token:          c.config.Token,
			Sniffer:        c.config.Sniffer,
			WebPort:        c.config.WebPort,
			SnifferLog:     c.config.SnifferLog,
			AggressivePool: c.config.AggressivePool,
		}
		tcpClient := transport.NewTCPClient(c.ctx, tcpConfig, c.logger)
		go tcpClient.Start()

	case config.TCPMUX:
		tcpMuxConfig := &transport.TcpMuxConfig{
			RemoteAddr:       c.config.RemoteAddr,
			Nodelay:          c.config.Nodelay,
			KeepAlive:        time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval:    time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:      time.Duration(c.config.DialTimeout) * time.Second,
			ConnPoolSize:     c.config.ConnectionPool,
			Token:            c.config.Token,
			MuxVersion:       c.config.MuxVersion,
			MaxFrameSize:     c.config.MaxFrameSize,
			MaxReceiveBuffer: c.config.MaxReceiveBuffer,
			MaxStreamBuffer:  c.config.MaxStreamBuffer,
			Sniffer:          c.config.Sniffer,
			WebPort:          c.config.WebPort,
			SnifferLog:       c.config.SnifferLog,
			AggressivePool:   c.config.AggressivePool,
		}
		tcpMuxClient := transport.NewMuxClient(c.ctx, tcpMuxConfig, c.logger)
		go tcpMuxClient.Start()

	case config.WS, config.WSS:
		wsConfig := &transport.WsConfig{
			RemoteAddr:     c.config.RemoteAddr,
			Nodelay:        c.config.Nodelay,
			KeepAlive:      time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval:  time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:    time.Duration(c.config.DialTimeout) * time.Second,
			ConnPoolSize:   c.config.ConnectionPool,
			Token:          c.config.Token,
			Sniffer:        c.config.Sniffer,
			WebPort:        c.config.WebPort,
			SnifferLog:     c.config.SnifferLog,
			Mode:           c.config.Transport,
			AggressivePool: c.config.AggressivePool,
			EdgeIP:         c.config.EdgeIP,
		}
		wsClient := transport.NewWSClient(c.ctx, wsConfig, c.logger)
		go wsClient.Start()

	case config.WSMUX, config.WSSMUX:
		wsMuxConfig := &transport.WsMuxConfig{
			RemoteAddr:       c.config.RemoteAddr,
			Nodelay:          c.config.Nodelay,
			KeepAlive:        time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval:    time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:      time.Duration(c.config.DialTimeout) * time.Second,
			ConnPoolSize:     c.config.ConnectionPool,
			Token:            c.config.Token,
			MuxVersion:       c.config.MuxVersion,
			MaxFrameSize:     c.config.MaxFrameSize,
			MaxReceiveBuffer: c.config.MaxReceiveBuffer,
			MaxStreamBuffer:  c.config.MaxStreamBuffer,
			Sniffer:          c.config.Sniffer,
			WebPort:          c.config.WebPort,
			SnifferLog:       c.config.SnifferLog,
			Mode:             c.config.Transport,
			AggressivePool:   c.config.AggressivePool,
			EdgeIP:           c.config.EdgeIP,
		}
		wsMuxClient := transport.NewWSMuxClient(c.ctx, wsMuxConfig, c.logger)
		go wsMuxClient.Start()

	case config.QUIC:
		quicConfig := &transport.QuicConfig{
			RemoteAddr:     c.config.RemoteAddr,
			Nodelay:        c.config.Nodelay,
			KeepAlive:      time.Duration(c.config.Keepalive) * time.Second,
			RetryInterval:  time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:    time.Duration(c.config.DialTimeout) * time.Second,
			ConnectionPool: c.config.ConnectionPool,
			Token:          c.config.Token,
			Sniffer:        c.config.Sniffer,
			WebPort:        c.config.WebPort,
			SnifferLog:     c.config.SnifferLog,
			AggressivePool: c.config.AggressivePool,
		}
		quicClient := transport.NewQuicClient(c.ctx, quicConfig, c.logger)
		go quicClient.ChannelDialer(true)

	case config.UDP:
		udpConfig := &transport.UdpConfig{
			RemoteAddr:    c.config.RemoteAddr,
			RetryInterval: time.Duration(c.config.RetryInterval) * time.Second,
			DialTimeOut:   time.Duration(c.config.DialTimeout) * time.Second,
			ConnPoolSize:  c.config.ConnectionPool,
			Token:         c.config.Token,
			Sniffer:       c.config.Sniffer,
			WebPort:       c.config.WebPort,
			SnifferLog:    c.config.SnifferLog,
			AggressivePool: c.config.AggressivePool,
		}
		udpClient := transport.NewUDPClient(c.ctx, udpConfig, c.logger)
		go udpClient.Start()

	default:
		c.logger.Fatalf("invalid transport type: %s", c.config.Transport)
	}

	<-c.ctx.Done()

	c.logger.Info("all workers stopped successfully")
	// suppress other logs after stopping
	c.logger.SetLevel(logrus.FatalLevel)
}

func (c *Client) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
}
