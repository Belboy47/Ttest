package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/musix/backhaul/cmd"
	"github.com/musix/backhaul/internal/utils"
)

var (
	logger     = utils.NewLogger("info")
	configPath *string
	ctx        context.Context
	cancel     context.CancelFunc
)

const version = "v0.6.6-custom"

func main() {
	configPath = flag.String("c", "", "path to the configuration file (TOML format)")
	showVersion := flag.Bool("v", false, "print the version and exit")
	maxProcs := flag.Int("procs", runtime.NumCPU(), "override maximum number of OS threads")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *configPath == "" {
		logger.Fatalf("Usage: %s -c /path/to/config.toml", flag.CommandLine.Name())
	}

	// Apply performance optimizations
	runtime.GOMAXPROCS(*maxProcs)
	cmd.ApplyTCPTuning()

	ctx, cancel = context.WithCancel(context.Background())

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)

	logger.Infof("Starting Backhaul %s with %d threads", version, *maxProcs)
	go cmd.Run(*configPath, ctx)
	go watchConfigAndReload()

	for sig := range sigChan {
		if sig == syscall.SIGHUP {
			logger.Warn("Received SIGHUP: triggering config reload")
			cancel()
			time.Sleep(1 * time.Second)
			ctx, cancel = context.WithCancel(context.Background())
			go cmd.Run(*configPath, ctx)
		} else {
			logger.Warnf("Signal %v received: shutting down", sig)
			cancel()
			time.Sleep(1 * time.Second)
			os.Exit(0)
		}
	}
}

func watchConfigAndReload() {
	lastModTime, err := getLastModTime(*configPath)
	if err != nil {
		logger.Fatalf("Error checking config file time: %v", err)
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			modTime, err := getLastModTime(*configPath)
			if err != nil {
				logger.Errorf("Config file stat error: %v", err)
				continue
			}
			if modTime.After(lastModTime) {
				logger.Info("Detected config change — reloading")
				cancel()
				time.Sleep(1 * time.Second)
				ctx, cancel = context.WithCancel(context.Background())
				go cmd.Run(*configPath, ctx)
				lastModTime = modTime
			}
		}
	}
}

func getLastModTime(file string) (time.Time, error) {
	absPath, _ := filepath.Abs(file)
	info, err := os.Stat(absPath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}
