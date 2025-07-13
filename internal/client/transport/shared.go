package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/musix/backhaul/internal/config"
	"golang.org/x/exp/rand"
)

// Seed random once at package init for user-agent randomization
func init() {
	rand.Seed(uint64(time.Now().UnixNano()))
}

// ResolveRemoteAddr splits and validates remote address string
func ResolveRemoteAddr(remoteAddr string) (int, string, error) {
	parts := strings.Split(remoteAddr, ":")
	var port int
	var err error

	if len(parts) < 2 {
		port, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, "", fmt.Errorf("invalid port format: %v", err)
		}
		return port, fmt.Sprintf("127.0.0.1:%d", port), nil
	}

	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", fmt.Errorf("invalid port format: %v", err)
	}

	return port, remoteAddr, nil
}

// TcpDialer attempts TCP connection with retry and exponential backoff
func TcpDialer(ctx context.Context, address string, timeout, keepAlive time.Duration, nodelay bool, retry, soRcvBuf, soSndBuf int) (*net.TCPConn, error) {
	if retry < 1 {
		retry = 1
	}

	backoff := 500 * time.Millisecond
	var lastErr error

	for i := 0; i < retry; i++ {
		tcpConn, err := attemptTcpDialer(ctx, address, timeout, keepAlive, nodelay, soRcvBuf, soSndBuf)
		if err == nil {
			return tcpConn, nil
		}

		lastErr = err

		if i == retry-1 {
			break
		}

		// Optional: add logging here, e.g.
		// fmt.Printf("[TcpDialer] attempt %d/%d failed: %v. Retrying in %v\n", i+1, retry, err, backoff)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}

	return nil, lastErr
}

// attemptTcpDialer performs a single TCP dial attempt with socket options
func attemptTcpDialer(ctx context.Context, address string, timeout, keepAlive time.Duration, nodelay bool, soRcvBuf, soSndBuf int) (*net.TCPConn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution: %v", err)
	}

	if soRcvBuf == 0 {
		soRcvBuf = 4 * 1024 * 1024 // 4MB default
	}
	if soSndBuf == 0 {
		soSndBuf = 4 * 1024 * 1024
	}

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: keepAlive,
		Control: func(network, address string, s syscall.RawConn) error {
			var controlErr error

			if err := ReusePortControl(network, address, s); err != nil {
				return err
			}

			if soRcvBuf > 0 {
				if err := s.Control(func(fd uintptr) {
					if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, soRcvBuf); err != nil {
						controlErr = fmt.Errorf("failed to set SO_RCVBUF: %v", err)
					}
				}); err != nil {
					return err
				}
			}

			if soSndBuf > 0 {
				if err := s.Control(func(fd uintptr) {
					if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, soSndBuf); err != nil {
						controlErr = fmt.Errorf("failed to set SO_SNDBUF: %v", err)
					}
				}); err != nil {
					return err
				}
			}

			return controlErr
		},
	}

	conn, err := dialer.DialContext(ctx, "tcp", tcpAddr.String())
	if err != nil {
		return nil, err
	}

	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		conn.Close()
		return nil, fmt.Errorf("failed to convert net.Conn to *net.TCPConn")
	}

	// Enable TCP_NODELAY based on nodelay flag
	if err := tcpConn.SetNoDelay(nodelay); err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("failed to set TCP_NODELAY: %v", err)
	}

	return tcpConn, nil
}

// ReusePortControl sets SO_REUSEADDR and optionally SO_REUSEPORT on Linux
func ReusePortControl(network, address string, s syscall.RawConn) error {
	var controlErr error

	err := s.Control(func(fd uintptr) {
		if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
			controlErr = fmt.Errorf("failed to set SO_REUSEADDR: %v", err)
			return
		}

		if runtime.GOOS == "linux" {
			const SO_REUSEPORT = 0xf
			if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, SO_REUSEPORT, 1); err != nil {
				controlErr = fmt.Errorf("failed to set SO_REUSEPORT: %v", err)
				return
			}
		}
	})

	if err != nil {
		return err
	}
	return controlErr
}

// WebSocketDialer attempts WebSocket connection with retries and exponential backoff
func WebSocketDialer(ctx context.Context, addr, edgeIP, path string, timeout, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, retry, soRcvBuf, soSndBuf int) (*websocket.Conn, error) {
	if retry < 1 {
		retry = 1
	}

	backoff := 500 * time.Millisecond
	var lastErr error

	for i := 0; i < retry; i++ {
		conn, err := attemptDialWebSocket(ctx, addr, edgeIP, path, timeout, keepalive, nodelay, token, mode, soRcvBuf, soSndBuf)
		if err == nil {
			return conn, nil
		}

		lastErr = err

		if i == retry-1 {
			break
		}

		// Optional: log retry
		time.Sleep(backoff)
		backoff *= 2
		if backoff > 10*time.Second {
			backoff = 10 * time.Second
		}
	}

	return nil, lastErr
}

func attemptDialWebSocket(ctx context.Context, addr, edgeIP, path string, timeout, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, soRcvBuf, soSndBuf int) (*websocket.Conn, error) {
	randomUserID := rand.Int31()

	userAgents := []string{
		// Chrome
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 12; Pixel 5) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Mobile Safari/537.36",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Safari/537.36",
		"Mozilla/5.0 (Linux; Android 9; SM-G960F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/108.0.5359.128 Mobile Safari/537.36",
		// Firefox
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:114.0) Gecko/20100101 Firefox/114.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:102.0) Gecko/20100101 Firefox/102.0",
		"Mozilla/5.0 (X11; Ubuntu; Linux x86_64; rv:115.0) Gecko/20100101 Firefox/115.0",
		"Mozilla/5.0 (Linux; Android 10; Pixel 4 XL) Gecko/20100101 Firefox/96.0",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 14_6 like Mac OS X) Gecko/20100101 Firefox/90.0",
		// Safari
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 11_4_1) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.0 Safari/605.1.15",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 15_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/15.1 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (iPad; CPU OS 14_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/14.0 Mobile/15E148 Safari/604.1",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/13.1.2 Safari/605.1.15",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		// Edge
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36 Edg/91.0.864.64",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/95.0.4638.69 Safari/537.36 Edg/95.0.1020.40",
		"Mozilla/5.0 (Linux; Android 11; SM-G998U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/90.0.4430.210 Mobile Safari/537.36 EdgA/46.3.4.5155",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/111.0.0.0 Safari/537.36 Edg/111.0.1661.44",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/115.0.0.0 Safari/537.36 Edg/115.0.1901.183",
		// Opera
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/112.0.0.0 Safari/537.36 OPR/97.0.4719.63",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36 OPR/98.0.4759.15",
		"Mozilla/5.0 (Linux; Android 10; SM-N975F) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/113.0.0.0 Mobile Safari/537.36 OPR/65.2.3381.61420",
		"Mozilla/5.0 (Linux; Android 11; SM-G998U) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.5735.196 Mobile Safari/537.36 OPR/71.2.3767.68577",
		"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/116.0.0.0 Safari/537.36 OPR/99.0.4759.21",
		// Older Browsers
		"Mozilla/4.0 (compatible; MSIE 9.0; Windows NT 6.1; Trident/5.0)",
		"Mozilla/4.0 (compatible; MSIE 6.0; Windows NT 5.1; SV1)",
		"Mozilla/5.0 (Windows NT 6.1; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/89.0.4389.82 Safari/537.36",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/87.0.4280.88 Safari/537.36",
	}

	randomUserAgent := userAgents[rand.Intn(len(userAgents))]

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("X-User-Id", strconv.Itoa(int(randomUserID)))
	headers.Set("User-Agent", randomUserAgent)

	if edgeIP != "" {
		_, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid addr format: %w", err)
		}
		edgeIP = net.JoinHostPort(edgeIP, port)
	} else {
		edgeIP = addr
	}

	if path != "/channel" {
		path = path + "/" + strconv.Itoa(int(randomUserID))
	}

	var wsURL string
	netDialFunc := func(_, address string) (net.Conn, error) {
		return TcpDialer(ctx, edgeIP, timeout, keepalive, nodelay, 1, soRcvBuf, soSndBuf)
	}

	switch mode {
	case config.WS, config.WSMUX:
		wsURL = "ws://" + addr + path
	case config.WSS, config.WSSMUX:
		wsURL = "wss://" + addr + path
	default:
		return nil, fmt.Errorf("unsupported transport mode: %v", mode)
	}

	dialer := websocket.Dialer{
		EnableCompression: true,
		HandshakeTimeout:  45 * time.Second,
		NetDial:           netDialFunc,
	}

	if mode == config.WSS || mode == config.WSSMUX {
		dialer.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	conn, _, err := dialer.Dial(wsURL, headers)
	if err != nil {
		return nil, fmt.Errorf("websocket dial error: %w", err)
	}

	return conn, nil
}
