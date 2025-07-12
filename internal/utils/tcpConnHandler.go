package utils

import (
	"errors"
	"io"
	"net"
	"syscall"
	"time"

	"github.com/musix/backhaul/internal/web"
	"github.com/sirupsen/logrus"
)

func TCPConnectionHandler(from net.Conn, to net.Conn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	setTCPOptions(from, logger)
	setTCPOptions(to, logger)
	done := make(chan struct{})

	go func() {
		defer close(done)
		transferData(from, to, logger, usage, remotePort, sniffer)
	}()

	transferData(to, from, logger, usage, remotePort, sniffer)

	<-done
}

func transferData(from net.Conn, to net.Conn, logger *logrus.Logger, usage *web.Usage, remotePort int, sniffer bool) {
	buf := make([]byte, 32*1024) // 32KB buffer for better throughput
	for {
		r, err := from.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				logger.Trace("reader stream closed or EOF received")
			} else {
				logger.Trace("unable to read from the connection: ", err)
			}
			from.Close()
			to.Close()
			return
		}

		totalWritten := 0
		for totalWritten < r {
			w, err := to.Write(buf[totalWritten:r])
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					logger.Trace("writer stream closed or EOF received")
				} else {
					logger.Trace("unable to write to the connection: ", err)
				}
				from.Close()
				to.Close()
				return
			}
			totalWritten += w
		}

		logger.Tracef("read %d bytes, wrote %d bytes", r, totalWritten)
		if sniffer {
			usage.AddOrUpdatePort(remotePort, uint64(totalWritten))
		}
	}
}

func setTCPOptions(conn net.Conn, logger *logrus.Logger) {
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}

	if err := tcpConn.SetKeepAlive(true); err != nil {
		logger.Debug("failed to enable SO_KEEPALIVE: ", err)
	}
	if err := tcpConn.SetKeepAlivePeriod(45 * time.Second); err != nil {
		logger.Debug("failed to set keepalive period: ", err)
	}
	if err := tcpConn.SetNoDelay(true); err != nil {
		logger.Debug("failed to enable TCP_NODELAY: ", err)
	}

	rawConn, err := tcpConn.SyscallConn()
	if err != nil {
		logger.Debug("failed to get syscall connection: ", err)
		return
	}

	rawConn.Control(func(fd uintptr) {
		linger := syscall.Linger{
			Onoff:  1,
			Linger: 2,
		}
		syscall.SetsockoptLinger(int(fd), syscall.SOL_SOCKET, syscall.SO_LINGER, &linger)
	})
}
