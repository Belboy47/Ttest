package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/musix/backhaul/cmd"
	"github.com/musix/backhaul/internal/utils"
)

var (
	logger      = utils.NewLogger("debug") // use "debug" level by default
	configPath  *string
	ctx         context.Context
	cancel      context.CancelFunc
	errorCount  int32 = 0                   // counts consecutive failures
	maxErrors   int32 = 3                   // threshold before forced shutdown
	checkPeriod       = 5 * time.Second     // watchdog check interval
)

const (
	version = "v0.6.6"
)

func main() {
	// CLI flags
	configPath = flag.String("c", "", "path to the configuration file (TOML format)")
	showVersion := flag.Bool("v", false, "print the version and exit")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *configPath == "" {
		logger.Fatalf("Usage: %s -c /path/to/config.toml", flag.CommandLine.Name())
	}

	// Apply basic TCP tuning at startup (from cmd package)
	cmd.ApplyTCPTuning()

	// Context setup
	ctx, cancel = context.WithCancel(context.Background())

	// Signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Start main runner
	go cmd.Run(*configPath, ctx)

	// Optional: hot reload on config change
	go hotReload()

	// New: start watchdog to monitor repeated errors
	go tunnelErrorWatchdog()

	logger.Info("Backhaul started successfully")
	<-sigChan

	cancel()
	logger.Info("Shutdown signal received, exiting")
	time.Sleep(1 * time.Second)
}

func tunnelErrorWatchdog() {
	ticker := time.NewTicker(checkPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			count := atomic.LoadInt32(&errorCount)
			if count > 0 {
				logger.Warnf("Tunnel error count: %d / %d", count, maxErrors)
			}
			if count >= maxErrors {
				logger.Errorf("Too many tunnel failures (>%d), exiting Backhaul to allow supervisor restart", maxErrors)
				os.Exit(1)
			}
		}
	}
}

// This should be called from inside tunnel failure paths
func incrementTunnelError(reason string) {
	newVal := atomic.AddInt32(&errorCount, 1)
	logger.Warnf("Tunnel error #%d: %s", newVal, reason)
}

// Optionally call this on successful tunnel flow to reset counter
func resetTunnelErrorCounter() {
	atomic.StoreInt32(&errorCount, 0)
}

func hotReload() {
	lastModTime, err := getLastModTime(*configPath)
	if err != nil {
		logger.Fatalf("Error getting config modification time: %v", err)
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			modTime, err := getLastModTime(*configPath)
			if err != nil {
				logger.Errorf("Error checking file modification time: %v", err)
				continue
			}

			if modTime.After(lastModTime) {
				logger.Info("Config file changed, reloading application")

				cancel()
				time.Sleep(2 * time.Second)

				newCtx, newCancel := context.WithCancel(context.Background())
				go cmd.Run(*configPath, newCtx)

				lastModTime = modTime
				ctx = newCtx
				cancel = newCancel
			}
		}
	}
}

func getLastModTime(file string) (time.Time, error) {
	absPath, _ := filepath.Abs(file)
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return time.Time{}, err
	}
	return fileInfo.ModTime(), nil
}
