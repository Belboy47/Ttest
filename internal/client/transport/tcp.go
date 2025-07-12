package transport

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config          *TcpConfig
	parentctx       context.Context
	ctx             context.Context
	cancel          context.CancelFunc
	logger          *logrus.Logger
	controlChannel  net.Conn
	usageMonitor    *web.Usage
	restartMutex    sync.Mutex
	poolConnections int32
	loadConnections int32
	controlFlow     chan struct{}
}

type TcpConfig struct {
	RemoteAddr     string
	Token          string
	SnifferLog     string
	TunnelStatus   string
	KeepAlive      time.Duration
	RetryInterval  time.Duration
	DialTimeOut    time.Duration
	ConnPoolSize   int
	WebPort        int
	Nodelay        bool
	Sniffer        bool
	AggressivePool bool
}

func NewTCPClient(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	ctx, cancel := context.WithCancel(parentCtx)
	return &TcpTransport{
		config:       config,
		parentctx:    parentCtx,
		ctx:          ctx,
		cancel:       cancel,
		logger:       logger,
		controlFlow:  make(chan struct{}, 100),
		usageMonitor: web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}
}

func (c *TcpTransport) Start() {
	if c.config.WebPort > 0 {
		go c.usageMonitor.Monitor()
	}
	c.config.TunnelStatus = "Disconnected (TCP)"
	go c.channelDialer()
}

func (c *TcpTransport) Restart() {
	if !c.restartMutex.TryLock() {
		c.logger.Warn("client is already restarting")
		return
	}
	defer c.restartMutex.Unlock()

	c.logger.Warn("restarting TCP client...")
	if c.cancel != nil {
		c.cancel()
	}
	if c.controlChannel != nil {
		c.controlChannel.Close()
	}
	time.Sleep(1 * time.Second)

	ctx, cancel := context.WithCancel(c.parentctx)
	*c = *NewTCPClient(c.parentctx, c.config, c.logger)
	c.ctx = ctx
	c.cancel = cancel
	go c.Start()
}

func (c *TcpTransport) channelDialer() {
	c.logger.Info("attempting to establish control channel...")

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			tunnelTCPConn, err := TcpDialer(c.ctx, c.config.RemoteAddr, c.config.DialTimeOut, c.config.KeepAlive, true, 3, 512*1024, 512*1024)
			if err != nil {
				c.logger.WithError(err).Error("failed dialing control channel")
				time.Sleep(c.config.RetryInterval)
				continue
			}
			tcpConn := tunnelTCPConn.(*net.TCPConn)
			tcpConn.SetLinger(0)
			tcpConn.SetKeepAlive(true)

			if err := utils.SendBinaryTransportString(tcpConn, c.config.Token, utils.SG_Chan); err != nil {
				c.logger.WithError(err).Error("failed to send token")
				tcpConn.Close()
				continue
			}

			tcpConn.SetReadDeadline(time.Now().Add(3 * time.Second))
			msg, _, err := utils.ReceiveBinaryTransportString(tcpConn)
			tcpConn.SetReadDeadline(time.Time{})

			if err != nil {
				c.logger.WithError(err).Error("control channel read failed")
				tcpConn.Close()
				time.Sleep(c.config.RetryInterval)
				continue
			}

			if msg != c.config.Token {
				c.logger.Warnf("unexpected token from server: %s", msg)
				tcpConn.Close()
				time.Sleep(c.config.RetryInterval)
				continue
			}

			c.controlChannel = tcpConn
			c.logger.Info("control channel established")
			c.config.TunnelStatus = "Connected (TCP)"
			go c.poolMaintainer()
			go c.channelHandler()
			return
		}
	}
}

func (c *TcpTransport) poolMaintainer() {
	for i := 0; i < c.config.ConnPoolSize; i++ {
		go func() {
			time.Sleep(time.Duration(i*20) * time.Millisecond) // stagger
			c.tunnelDialer()
		}()
	}

	a, b, x, y := 4, 5, 3, 4.0
	if c.config.AggressivePool {
		a, b, x, y = 1, 2, 0, 0.75
		c.logger.Info("aggressive pool management enabled")
	}
	tPool := time.NewTicker(1 * time.Second)
	tLoad := time.NewTicker(10 * time.Second)
	defer tPool.Stop()
	defer tLoad.Stop()

	newSize := c.config.ConnPoolSize
	var sum int32

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-tPool.C:
			atomic.AddInt32(&sum, atomic.LoadInt32(&c.poolConnections))
		case <-tLoad.C:
			load := (int(atomic.LoadInt32(&c.loadConnections)) + 9) / 10
			pool := (int(atomic.LoadInt32(&sum)) + 9) / 10
			atomic.StoreInt32(&sum, 0)
			atomic.StoreInt32(&c.loadConnections, 0)

			if (load + a) > pool*b {
				newSize++
				go c.tunnelDialer()
			} else if float64(load+x) < float64(pool)*y && newSize > c.config.ConnPoolSize {
				newSize--
				c.controlFlow <- struct{}{}
			}
		}
	}
}

func (c *TcpTransport) tunnelDialer() {
	c.logger.Debugf("initiating new connection to tunnel server at %s", c.config.RemoteAddr)
	tcpConn, err := TcpDialer(c.ctx, c.config.RemoteAddr, c.config.DialTimeOut, c.config.KeepAlive, c.config.Nodelay, 3, 1024*1024, 1024*1024)
	if err != nil {
		c.logger.WithError(err).Error("tunnel server dial error")
		return
	}
	atomic.AddInt32(&c.poolConnections, 1)

	remoteAddr, transport, err := utils.ReceiveBinaryTransportString(tcpConn)
	atomic.AddInt32(&c.poolConnections, -1)
	if err != nil {
		c.logger.WithError(err).Warnf("recv failed from %s", tcpConn.RemoteAddr().String())
		tcpConn.Close()
		return
	}

	if f, ok := tcpConn.(*net.TCPConn); ok {
		f.SetLinger(0)
		f.SetKeepAlive(true)
	}

	port, addr, err := ResolveRemoteAddr(remoteAddr)
	if err != nil {
		c.logger.WithError(err).Warn("could not resolve tunnel address")
		tcpConn.Close()
		return
	}

	switch transport {
	case utils.SG_TCP:
		c.localDialer(tcpConn, addr, port)
	case utils.SG_UDP:
		UDPDialer(tcpConn, addr, c.logger, c.usageMonitor, port, c.config.Sniffer)
	default:
		c.logger.Error("invalid transport type")
		tcpConn.Close()
	}
}

func (c *TcpTransport) localDialer(tcpConn net.Conn, addr string, port int) {
	localConn, err := TcpDialer(c.ctx, addr, c.config.DialTimeOut, c.config.KeepAlive, true, 1, 32*1024, 32*1024)
	if err != nil {
		c.logger.WithError(err).Error("local dialer failed")
		tcpConn.Close()
		return
	}
	c.logger.Debugf("connected to local address %s successfully", addr)
	utils.TCPConnectionHandler(tcpConn, localConn, c.logger, c.usageMonitor, port, c.config.Sniffer)
}

func (c *TcpTransport) channelHandler() {
	msgChan := make(chan byte, 1000)
	go func() {
		for {
			select {
			case <-c.ctx.Done():
				return
			default:
				msg, err := utils.ReceiveBinaryByte(c.controlChannel)
				if err != nil {
					c.logger.WithError(err).Error("control channel read failed")
					go c.Restart()
					return
				}
				msgChan <- msg
			}
		}
	}()

	for {
		select {
		case <-c.ctx.Done():
			_ = utils.SendBinaryByte(c.controlChannel, utils.SG_Closed)
			return
		case msg := <-msgChan:
			switch msg {
			case utils.SG_Chan:
				atomic.AddInt32(&c.loadConnections, 1)
				select {
				case <-c.controlFlow:
				default:
					c.logger.Debug("channel signal received, initiating tunnel dialer")
					go c.tunnelDialer()
				}
			case utils.SG_HB:
				c.logger.Debug("heartbeat received")
			case utils.SG_Closed:
				c.logger.Warn("server closed control channel")
				go c.Restart()
				return
			case utils.SG_RTT:
				_ = utils.SendBinaryByte(c.controlChannel, utils.SG_RTT)
			default:
				c.logger.Errorf("unexpected signal byte: %v", msg)
				go c.Restart()
				return
			}
		}
	}
}
