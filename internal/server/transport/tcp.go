package transport

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
)

type TcpTransport struct {
	config         *TcpConfig
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChannel  chan net.Conn
	localChannel   chan LocalTCPConn
	reqNewConnChan chan struct{}
	controlChannel net.Conn
	restartMutex   sync.Mutex
	usageMonitor   *web.Usage
	rtt            int64 // in ms, for UDP
}

type TcpConfig struct {
	BindAddr     string
	Token        string
	SnifferLog   string
	TunnelStatus string
	Ports        []string
	Nodelay      bool          // true = enable TCP_NODELAY, false = disable
	Sniffer      bool
	KeepAlive    time.Duration // e.g. 30s or 1m
	Heartbeat    time.Duration // in seconds
	ChannelSize  int
	WebPort      int
	AcceptUDP    bool
}

// Default socket buffer sizes for TCP connections (2MB)
const defaultSockBufSize = 2 * 1024 * 1024

func NewTCPServer(parentCtx context.Context, config *TcpConfig, logger *logrus.Logger) *TcpTransport {
	ctx, cancel := context.WithCancel(parentCtx)

	server := &TcpTransport{
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChannel:  make(chan net.Conn, config.ChannelSize),
		localChannel:   make(chan LocalTCPConn, config.ChannelSize),
		reqNewConnChan: make(chan struct{}, config.ChannelSize),
		controlChannel: nil,
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
		rtt:            0,
	}

	return server
}

func (s *TcpTransport) Start() {
	s.config.TunnelStatus = "Disconnected (TCP)"

	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}

	go s.tunnelListener()

	s.channelHandshake()

	if s.controlChannel != nil {
		s.config.TunnelStatus = "Connected (TCP)"

		numCPU := runtime.NumCPU()
		if numCPU > 4 {
			numCPU = 4
		}

		go s.parsePortMappings()
		go s.channelHandler()

		s.logger.Infof("starting %d handle loops on each CPU thread", numCPU)

		for i := 0; i < numCPU; i++ {
			go s.handleLoop()
		}
	}
}

func (s *TcpTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")

	level := s.logger.Level
	s.logger.SetLevel(logrus.FatalLevel)

	if s.cancel != nil {
		s.cancel()
	}

	if s.controlChannel != nil {
		s.controlChannel.Close()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	s.tunnelChannel = make(chan net.Conn, s.config.ChannelSize)
	s.localChannel = make(chan LocalTCPConn, s.config.ChannelSize)
	s.reqNewConnChan = make(chan struct{}, s.config.ChannelSize)
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""
	s.controlChannel = nil

	s.logger.SetLevel(level)

	go s.Start()
}

func (s *TcpTransport) channelHandshake() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case conn := <-s.tunnelChannel:
			if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				s.logger.Errorf("failed to set read deadline: %v", err)
				conn.Close()
				continue
			}

			msg, transport, err := utils.ReceiveBinaryTransportString(conn)
			if transport != utils.SG_Chan {
				s.logger.Errorf("invalid signal received for channel, discarding connection")
				conn.Close()
				continue
			} else if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					s.logger.Warn("timeout while waiting for control channel signal")
				} else {
					s.logger.Errorf("failed to receive control channel signal: %v", err)
				}
				conn.Close()
				continue
			}

			conn.SetReadDeadline(time.Time{})

			if msg != s.config.Token {
				s.logger.Warnf("invalid security token received: %s", msg)
				conn.Close()
				continue
			}

			err = utils.SendBinaryTransportString(conn, s.config.Token, utils.SG_Chan)
			if err != nil {
				s.logger.Errorf("failed to send security token: %v", err)
				conn.Close()
				continue
			}

			s.controlChannel = conn

			s.logger.Info("control channel successfully established.")
			return
		}
	}
}

func (s *TcpTransport) channelHandler() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	messageChan := make(chan byte, 1)

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				message, err := utils.ReceiveBinaryByte(s.controlChannel)
				if err != nil {
					if s.cancel != nil {
						s.logger.Error("failed to read from channel connection. ", err)
						go s.Restart()
					}
					return
				}
				messageChan <- message
			}
		}
	}()

	rtt := time.Now()
	err := utils.SendBinaryByte(s.controlChannel, utils.SG_RTT)
	if err != nil {
		s.logger.Error("failed to send RTT signal, attempting to restart server...")
		go s.Restart()
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			_ = utils.SendBinaryByte(s.controlChannel, utils.SG_Closed)
			return

		case <-s.reqNewConnChan:
			err := utils.SendBinaryByte(s.controlChannel, utils.SG_Chan)
			if err != nil {
				s.logger.Error("failed to send request new connection signal. ", err)
				go s.Restart()
				return
			}

		case <-ticker.C:
			err := utils.SendBinaryByte(s.controlChannel, utils.SG_HB)
			if err != nil {
				s.logger.Error("failed to send heartbeat signal")
				go s.Restart()
				return
			}
			s.logger.Trace("heartbeat signal sent successfully")

		case message, ok := <-messageChan:
			if !ok {
				s.logger.Error("channel closed, likely due to an error in TCP read")
				return
			}

			if message == utils.SG_Closed {
				s.logger.Warn("control channel has been closed by the client")
				go s.Restart()
				return

			} else if message == utils.SG_RTT {
				measureRTT := time.Since(rtt)
				s.rtt = measureRTT.Milliseconds()
				s.logger.Infof("Round Trip Time (RTT): %d ms", s.rtt)
			}
		}
	}
}

// Modified tunnelListener with SO_REUSEADDR and SO_REUSEPORT
func (s *TcpTransport) tunnelListener() {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				if err != nil {
					return
				}
				if runtime.GOOS == "linux" {
					err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
				}
			})
			return err
		},
	}

	listener, err := lc.Listen(s.ctx, "tcp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", s.config.BindAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("server started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptTunnelConn(listener)

	<-s.ctx.Done()
}

func (s *TcpTransport) acceptTunnelConn(listener net.Listener) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
			s.logger.Debugf("waiting for accept incoming tunnel connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept tunnel connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("discarded non-TCP tunnel connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			if s.controlChannel != nil && s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String() != tcpConn.RemoteAddr().(*net.TCPAddr).IP.String() {
				s.logger.Debugf("suspicious packet from %v. expected address: %v. discarding packet...", tcpConn.RemoteAddr().(*net.TCPAddr).IP.String(), s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String())
				tcpConn.Close()
				continue
			}

			// TCP_NODELAY: enabled by default, disabled if config.Nodelay==false
			tcpNodelay := true
			if s.config.Nodelay == false {
				tcpNodelay = false
			}
			if err := tcpConn.SetNoDelay(tcpNodelay); err != nil {
				s.logger.Warnf("failed to set TCP_NODELAY=%v for %s: %v", tcpNodelay, tcpConn.RemoteAddr().String(), err)
			}

			// KeepAlive settings
			if err := tcpConn.SetKeepAlive(true); err != nil {
				s.logger.Warnf("failed to enable TCP keep-alive for %s: %v", tcpConn.RemoteAddr().String(), err)
			}
			if err := tcpConn.SetKeepAlivePeriod(s.config.KeepAlive); err != nil {
				s.logger.Warnf("failed to set TCP keep-alive period for %s: %v", tcpConn.RemoteAddr().String(), err)
			}

			// Set socket buffer sizes
			rawConn, err := tcpConn.SyscallConn()
			if err == nil {
				rawConn.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, defaultSockBufSize)
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, defaultSockBufSize)
				})
			} else {
				s.logger.Warnf("failed to get raw connection for socket options: %v", err)
			}

			select {
			case s.tunnelChannel <- conn:
			default:
				s.logger.Warnf("tunnel listener channel is full, discarding TCP connection from %s", conn.LocalAddr().String())
				conn.Close()
			}
		}
	}
}

func (s *TcpTransport) parsePortMappings() {
	for _, portMapping := range s.config.Ports {
		parts := strings.Split(portMapping, "=")

		var localAddr, remoteAddr string

		if len(parts) == 1 {
			localPortOrRange := strings.TrimSpace(parts[0])
			remoteAddr = localPortOrRange

			if strings.Contains(localPortOrRange, "-") {
				rangeParts := strings.Split(localPortOrRange, "-")
				if len(rangeParts) != 2 {
					s.logger.Fatalf("invalid port range format: %s", localPortOrRange)
				}

				startPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				if err != nil || startPort < 1 || startPort > 65535 {
					s.logger.Fatalf("invalid start port in range: %s", rangeParts[0])
				}

				endPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err != nil || endPort < 1 || endPort > 65535 || endPort < startPort {
					s.logger.Fatalf("invalid end port in range: %s", rangeParts[1])
				}

				for port := startPort; port <= endPort; port++ {
					localAddr = fmt.Sprintf(":%d", port)
					go s.startListeners(localAddr, strconv.Itoa(port))
					time.Sleep(1 * time.Millisecond)
				}
				continue
			} else {
				port, err := strconv.Atoi(localPortOrRange)
				if err != nil || port < 1 || port > 65535 {
					s.logger.Fatalf("invalid port format: %s", localPortOrRange)
				}
				localAddr = fmt.Sprintf(":%d", port)
			}
		} else if len(parts) == 2 {
			localPortOrRange := strings.TrimSpace(parts[0])
			remoteAddr = strings.TrimSpace(parts[1])

			if strings.Contains(localPortOrRange, "-") {
				rangeParts := strings.Split(localPortOrRange, "-")
				if len(rangeParts) != 2 {
					s.logger.Fatalf("invalid port range format: %s", localPortOrRange)
				}

				startPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				if err != nil || startPort < 1 || startPort > 65535 {
					s.logger.Fatalf("invalid start port in range: %s", rangeParts[0])
				}

				endPort, err := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				if err != nil || endPort < 1 || endPort > 65535 || endPort < startPort {
					s.logger.Fatalf("invalid end port in range: %s", rangeParts[1])
				}

				for port := startPort; port <= endPort; port++ {
					localAddr = fmt.Sprintf(":%d", port)
					go s.startListeners(localAddr, remoteAddr)
					time.Sleep(1 * time.Millisecond)
				}
				continue
			} else {
				port, err := strconv.Atoi(localPortOrRange)
				if err == nil && port > 1 && port < 65535 {
					localAddr = fmt.Sprintf(":%d", port)
				} else {
					localAddr = localPortOrRange
				}
			}
		} else {
			s.logger.Fatalf("invalid port mapping format: %s", portMapping)
		}

		go s.startListeners(localAddr, remoteAddr)
	}
}

func (s *TcpTransport) startListeners(localAddr, remoteAddr string) {
	go s.localListener(localAddr, remoteAddr)

	if s.config.AcceptUDP {
		go s.udpListener(localAddr, remoteAddr)
	}

	s.logger.Debugf("Started listening on %s, forwarding to %s", localAddr, remoteAddr)
}

func (s *TcpTransport) localListener(localAddr string, remoteAddr string) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				if err != nil {
					return
				}
				if runtime.GOOS == "linux" {
					err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEPORT, 1)
				}
			})
			return err
		},
	}

	listener, err := lc.Listen(s.ctx, "tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to listen on %s: %v", localAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	go s.acceptLocalConn(listener, remoteAddr)

	<-s.ctx.Done()
}

func (s *TcpTransport) acceptLocalConn(listener net.Listener, remoteAddr string) {
	for {
		select {
		case <-s.ctx.Done():
			return

		default:
			s.logger.Debugf("waiting for accept incoming connection on %s", listener.Addr().String())
			conn, err := listener.Accept()
			if err != nil {
				s.logger.Debugf("failed to accept connection on %s: %v", listener.Addr().String(), err)
				continue
			}

			tcpConn, ok := conn.(*net.TCPConn)
			if !ok {
				s.logger.Warnf("discarded non-TCP connection from %s", conn.RemoteAddr().String())
				conn.Close()
				continue
			}

			tcpNodelay := true
			if s.config.Nodelay == false {
				tcpNodelay = false
			}
			if err := tcpConn.SetNoDelay(tcpNodelay); err != nil {
				s.logger.Warnf("failed to set TCP_NODELAY=%v for %s: %v", tcpNodelay, tcpConn.RemoteAddr().String(), err)
			}

			select {
			case s.localChannel <- LocalTCPConn{conn: conn, remoteAddr: remoteAddr, timeCreated: time.Now().UnixMilli()}:

				select {
				case s.reqNewConnChan <- struct{}{}:
				default:
					s.logger.Warn("channel is full, cannot request a new connection")
				}

				s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

			default:
				s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
				conn.Close()
			}
		}
	}
}
func (s *TcpTransport) handleLoop() {
	for {
		select {
		case <-s.ctx.Done():
			s.logger.Info("handleLoop context done, exiting")
			return
		case conn := <-s.tunnelChannel:
			// Handle the incoming connection
			if conn == nil {
				continue
			}
			go s.handleConnection(conn)
		}
	}
}

// handleConnection processes a single TCP tunnel connection.
func (s *TcpTransport) handleConnection(conn net.Conn) {
	defer conn.Close()

	// Example: just log remote addr and echo back data (you can customize this)
	remoteAddr := conn.RemoteAddr().String()
	s.logger.Infof("Handling new tunnel connection from %s", remoteAddr)

	// Here you would implement your actual proxy/tunnel logic
	// For example, forwarding data between localChannel and this tunnel connection
	// This is a placeholder for your actual transport logic.

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err != net.ErrClosed {
				s.logger.Warnf("connection from %s closed or error: %v", remoteAddr, err)
			}
			return
		}
		// Echo back (remove or replace with actual logic)
		_, err = conn.Write(buf[:n])
		if err != nil {
			s.logger.Warnf("failed to write back to %s: %v", remoteAddr, err)
			return
		}
	}
}
