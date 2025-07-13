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

// Seed random once at package init for WebSocket user-agent selection
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
func TcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, retry int, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
	var tcpConn *net.TCPConn
	var err error

	// Set default buffer sizes if zero
	if SO_RCVBUF == 0 {
		SO_RCVBUF = 4 * 1024 * 1024 // 4MB receive buffer
	}
	if SO_SNDBUF == 0 {
		SO_SNDBUF = 4 * 1024 * 1024 // 4MB send buffer
	}

	retries := retry
	backoff := 1 * time.Second

	for i := 0; i < retries; i++ {
		tcpConn, err = attemptTcpDialer(ctx, address, timeout, keepAlive, nodelay, SO_RCVBUF, SO_SNDBUF)
		if err == nil {
			return tcpConn, nil
		}

		// Log retry attempt
		fmt.Printf("[TcpDialer] dial to %s failed: %v. Retrying in %v (%d/%d)\n", address, err, backoff, i+1, retries)

		if i == retries-1 {
			break
		}

		time.Sleep(backoff)
		backoff *= 2
	}

	return nil, err
}

// attemptTcpDialer performs a single TCP dial attempt with socket options
func attemptTcpDialer(ctx context.Context, address string, timeout time.Duration, keepAlive time.Duration, nodelay bool, SO_RCVBUF int, SO_SNDBUF int) (*net.TCPConn, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("DNS resolution: %v", err)
	}

	dialer := &net.Dialer{
		Control: func(network, address string, s syscall.RawConn) error {
			err := ReusePortControl(network, address, s)
			if err != nil {
				return err
			}

			if SO_RCVBUF > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, SO_RCVBUF); err != nil {
						err = fmt.Errorf("failed to set SO_RCVBUF: %v", err)
					}
				})
			}
			if err != nil {
				return err
			}

			if SO_SNDBUF > 0 {
				err = s.Control(func(fd uintptr) {
					if err = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, SO_SNDBUF); err != nil {
						err = fmt.Errorf("failed to set SO_SNDBUF: %v", err)
					}
				})
			}

			return err
		},
		Timeout:   timeout,
		KeepAlive: keepAlive,
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

	// Enable TCP_NODELAY by default unless explicitly disabled
	if nodelay {
		err = tcpConn.SetNoDelay(true)
	} else {
		err = tcpConn.SetNoDelay(false)
	}
	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("failed to set TCP_NODELAY")
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
			// SO_REUSEPORT is 0xf on Linux
			if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0xf, 1); err != nil {
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

// WebSocketDialer attempts to establish a WebSocket connection with retries and backoff
func WebSocketDialer(ctx context.Context, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, retry int, SO_RCVBUF int, SO_SNDBUF int) (*websocket.Conn, error) {
	var tunnelWSConn *websocket.Conn
	var err error

	// Set default buffer sizes if zero
	if SO_RCVBUF == 0 {
		SO_RCVBUF = 4 * 1024 * 1024
	}
	if SO_SNDBUF == 0 {
		SO_SNDBUF = 4 * 1024 * 1024
	}

	retries := retry
	backoff := 1 * time.Second

	for i := 0; i < retries; i++ {
		tunnelWSConn, err = attemptDialWebSocket(ctx, addr, edgeIP, path, timeout, keepalive, nodelay, token, mode, SO_RCVBUF, SO_SNDBUF)
		if err == nil {
			return tunnelWSConn, nil
		}

		fmt.Printf("[WebSocketDialer] dial to %s failed: %v. Retrying in %v (%d/%d)\n", addr, err, backoff, i+1, retries)

		if i == retries-1 {
			break
		}

		time.Sleep(backoff)
		backoff *= 2
	}

	return nil, err
}

// attemptDialWebSocket makes one WebSocket dial attempt
func attemptDialWebSocket(ctx context.Context, addr string, edgeIP string, path string, timeout time.Duration, keepalive time.Duration, nodelay bool, token string, mode config.TransportType, SO_RCVBUF int, SO_SNDBUF int) (*websocket.Conn, error) {
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
		"Mozilla/5.0 (Linux; Android 10; SM-N975F) AppleWebKit/537.36
	}
