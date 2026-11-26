package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/boumenot/heavy-lock/apps/tuna/cli"
	"github.com/boumenot/heavy-lock/apps/tuna/service"
	"github.com/boumenot/heavy-lock/apps/tuna/transport"
)

const (
	usageExitCode    = 64
	softwareExitCode = 70
	// tempFailExitCode (EX_TEMPFAIL) reports another broker holding the
	// per-user singleton lock, matching the runner's busy/unavailable code.
	tempFailExitCode = 75
	serviceVersion   = "2.0.0-dev"
	serviceUsage     = "Service: tuna service [--port PORT (default 49783)] [--data-dir DIR]\n" +
		"  [--event-max-age DURATION (default 24h)] [--event-max-count N (default 100000)]\n" +
		"  Serves unauthenticated control gRPC/REST on 127.0.0.1 only; a port conflict\n" +
		"  is an error (exit 70), never a different port; a second broker exits 75.\n" +
		"  Runner RPCs are not implemented yet.\n"
	usage = "Usage: tuna --port <port> [runner options] -- <command> [arguments...]\n" +
		"       tuna service [options]\n" +
		"       tuna <control> [options]\n\n" +
		"The v2 runner is not implemented yet.\n\n" + serviceUsage + "\n" + cli.Usage
)

func writeResult(w io.Writer, message string, code int) int {
	if _, err := io.WriteString(w, message); err != nil {
		return softwareExitCode
	}
	return code
}

func run(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runContext(ctx, args, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return writeResult(stdout, usage, 0)
	}
	if len(args) > 0 && args[0] == "service" {
		return runService(ctx, args[1:], stdout, stderr)
	}
	if code, handled := cli.Run(ctx, args, stdout, stderr); handled {
		return code
	}
	for i, arg := range args {
		if arg == "--" && i+1 < len(args) {
			if !hasExplicitPort(args[:i]) {
				return writeResult(stderr, "tuna: runner requires one --port between 1 and 65535\n", usageExitCode)
			}
			return writeResult(stderr, "tuna: runner mode is not implemented\n", softwareExitCode)
		}
	}
	return writeResult(stderr, usage, usageExitCode)
}

func runService(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", transport.DefaultPort, "broker port")
	dataDir := fs.String("data-dir", "", "broker state directory")
	maxAge := fs.Duration("event-max-age", 24*time.Hour, "event replay age limit")
	maxCount := fs.Int64("event-max-count", 100000, "event replay count limit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeResult(stdout, serviceUsage, 0)
		}
		return writeResult(stderr, fmt.Sprintf("tuna: %v\n%s", err, serviceUsage), usageExitCode)
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 || *maxAge <= 0 || *maxCount < 1 {
		return writeResult(stderr, "tuna: invalid service arguments\n"+serviceUsage, usageExitCode)
	}
	err := service.Run(ctx, service.Options{
		Port:           *port,
		DataDir:        *dataDir,
		ServiceVersion: serviceVersion,
		Log:            stderr,
		EventMaxAge:    *maxAge,
		EventMaxCount:  *maxCount,
	})
	if errors.Is(err, service.ErrLocked) {
		return writeResult(stderr, fmt.Sprintf("tuna service: %v\n", err), tempFailExitCode)
	}
	if err != nil {
		return writeResult(stderr, fmt.Sprintf("tuna service: %v\n", err), softwareExitCode)
	}
	return 0
}

func hasExplicitPort(args []string) bool {
	found := false
	for i := 0; i < len(args); i++ {
		var value string
		switch {
		case args[i] == "--port":
			i++
			if i == len(args) {
				return false
			}
			value = args[i]
		case strings.HasPrefix(args[i], "--port="):
			value = strings.TrimPrefix(args[i], "--port=")
		default:
			continue
		}
		port, err := strconv.Atoi(value)
		if found || err != nil || port < 1 || port > 65535 {
			return false
		}
		found = true
	}
	return found
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
