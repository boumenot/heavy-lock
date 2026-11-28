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

	"github.com/boumenot/tuna/apps/tuna/attribution"
	"github.com/boumenot/tuna/apps/tuna/cli"
	"github.com/boumenot/tuna/apps/tuna/contract"
	"github.com/boumenot/tuna/apps/tuna/runner"
	"github.com/boumenot/tuna/apps/tuna/scheduler"
	"github.com/boumenot/tuna/apps/tuna/service"
	"github.com/boumenot/tuna/apps/tuna/transport"
)

// version identifies this build to the broker and in client attribution.
var version = contract.ServiceVersion

const (
	usageExitCode    = runner.ExitUsage
	softwareExitCode = runner.ExitSoftware
	serviceUsage     = "Service: tuna service [--port PORT (default 49783)] [--data-dir DIR]\n" +
		"  [--capacity N (positive; default automatic)] [--backfill-limit K (default 1; 0 is FIFO)]\n" +
		"  [--event-max-age DURATION (default 24h)] [--event-max-count N (default 100000)]\n" +
		"  [--launch-timeout DURATION (default 30s)]\n" +
		"  [--cancel-ack-timeout DURATION (default 5s)]\n" +
		"  [--cancel-completion-timeout DURATION (default 30s)]\n" +
		"  Serves unauthenticated control gRPC/REST and the runner service on\n" +
		"  127.0.0.1 only; a port conflict is an error, never a different port.\n" +
		"  A second broker for the same user exits 75.\n"
	usage = "Usage: tuna --port <port> [runner options] -- <command> [arguments...]\n" +
		"       tuna service [options]\n" +
		"       tuna <control> [options]\n\n" +
		"Runner options:\n" +
		"  --port <port>          broker port on 127.0.0.1 (required)\n" +
		"  --weight <weight>      heavy (default), light, or a positive integer\n" +
		"  --priority <integer>   queue admission priority (default 1337; lower first)\n" +
		"  --repository <repo>    attribute the job to [HOST/]OWNER/NAME instead of\n" +
		"                         the working directory's git remote\n" +
		"  --worklog <issue>      attribute the job to worklog N or [HOST/]OWNER/NAME#N;\n" +
		"                         N belongs to the job's repository\n" +
		"  --no-wait              exit 75 instead of queueing when not admitted\n" +
		"  --retries <n>          retry an eligible nonzero exit up to n (0-3) times\n\n" +
		"The command runs without a shell. The broker must already be running;\n" +
		"an unreachable broker exits 69.\n\n" + serviceUsage + "\n" + cli.Usage
)

func writeResult(w io.Writer, message string, code int) int {
	if _, err := io.WriteString(w, message); err != nil {
		return softwareExitCode
	}
	return code
}

func usageError(stderr io.Writer, format string, args ...any) int {
	return writeResult(stderr, "tuna: "+fmt.Sprintf(format, args...)+"\n", usageExitCode)
}

// launcher runs a parsed invocation; tests substitute it.
type launcher struct {
	service func(service.Options) int
	runner  func(runner.Options) int
	control func(args []string, stdout, stderr io.Writer) (int, bool)
}

// serviceRunner is replaced only by the tuna_test_broker build.
var serviceRunner = service.Run

func run(args []string, stdout, stderr io.Writer) int {
	return runWithService(args, stdout, stderr, serviceRunner)
}

func runWithService(args []string, stdout, stderr io.Writer, runService func(context.Context, service.Options) error) int {
	return dispatch(args, stdout, stderr, launcher{
		service: func(o service.Options) int {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			err := runService(ctx, o)
			switch {
			case errors.Is(err, service.ErrLocked):
				_, _ = fmt.Fprintf(stderr, "tuna service: %v\n", err)
				return runner.ExitBusy
			case err != nil:
				_, _ = fmt.Fprintf(stderr, "tuna service: %v\n", err)
				return softwareExitCode
			}
			return 0
		},
		runner: func(o runner.Options) int {
			signals := make(chan os.Signal, 4)
			signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer signal.Stop(signals)
			o.Signals = signals
			return runner.Run(context.Background(), o)
		},
		control: func(args []string, stdout, stderr io.Writer) (int, bool) {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return cli.Run(ctx, args, stdout, stderr, version)
		},
	})
}

func dispatch(args []string, stdout, stderr io.Writer, l launcher) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return writeResult(stdout, usage, 0)
	}
	if len(args) > 0 && args[0] == "service" {
		return dispatchService(args[1:], stdout, stderr, l)
	}
	if code, handled := l.control(args, stdout, stderr); handled {
		return code
	}
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 || sep+1 == len(args) {
		return writeResult(stderr, usage, usageExitCode)
	}
	o := runner.Options{Command: args[sep+1:], Version: version, Log: stderr, Weight: runner.HeavyWeight}
	seen := map[string]bool{}
	for i := 0; i < sep; i++ {
		if args[i] == "--no-wait" {
			if seen[args[i]] {
				return usageError(stderr, "--no-wait may be given once")
			}
			seen[args[i]], o.NoWait = true, true
			continue
		}
		name, value, ok, err := option(args[:sep], &i, seen)
		if err != nil {
			return usageError(stderr, "%v", err)
		}
		if !ok {
			return usageError(stderr, "unknown runner option %q", args[i])
		}
		switch name {
		case "--port":
			o.Port, err = parsePort(value)
		case "--weight":
			var valid bool
			if o.Weight, valid = runner.ParseWeight(value); !valid {
				err = fmt.Errorf("--weight must be heavy, light or a positive integer")
			}
		case "--priority":
			var n int64
			n, err = strconv.ParseInt(value, 10, 32)
			if err != nil {
				err = fmt.Errorf("--priority must be a signed 32-bit integer")
			} else {
				priority := int32(n)
				o.Priority = &priority
			}
		case "--repository":
			var r attribution.Repository
			if r, err = attribution.ParseRepository(value); err == nil {
				o.Attribution.Repository = &r
			} else {
				err = fmt.Errorf("--%v", err)
			}
		case "--worklog":
			var w attribution.Worklog
			if w, err = attribution.ParseWorklog(value); err == nil {
				o.Attribution.Worklog = &w
			} else {
				err = fmt.Errorf("--%v", err)
			}
		case "--retries":
			n, perr := strconv.Atoi(value)
			if perr != nil || n < 0 || n > runner.MaxRetries {
				err = fmt.Errorf("--retries must be between 0 and %d", runner.MaxRetries)
			}
			o.Retries = n
		}
		if err != nil {
			return usageError(stderr, "%v", err)
		}
	}
	if !seen["--port"] {
		return usageError(stderr, "runner requires one --port between 1 and 65535")
	}
	return l.runner(o)
}

func dispatchService(args []string, stdout, stderr io.Writer, l launcher) int {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", transport.DefaultPort, "broker port")
	dataDir := fs.String("data-dir", "", "broker state directory")
	var capacity uint32
	var backfillLimit *uint32
	fs.Func("capacity", "positive broker capacity", func(value string) error {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n == 0 {
			return errors.New("capacity must be a positive 32-bit integer")
		}
		capacity = uint32(n)
		return nil
	})
	fs.Func("backfill-limit", "maximum bypasses per head", func(value string) error {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			return errors.New("backfill limit must be a non-negative 32-bit integer")
		}
		limit := uint32(n)
		backfillLimit = &limit
		return nil
	})
	maxAge := fs.Duration("event-max-age", 24*time.Hour, "event replay age limit")
	maxCount := fs.Int64("event-max-count", 100000, "event replay count limit")
	launchTimeout := fs.Duration("launch-timeout", scheduler.LaunchTimeout, "launch investigation timeout")
	cancelAckTimeout := fs.Duration("cancel-ack-timeout", scheduler.CancelAckTimeout, "cancellation acknowledgment timeout")
	cancelCompletionTimeout := fs.Duration("cancel-completion-timeout", scheduler.CancelCompletionTimeout, "cancellation completion timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeResult(stdout, serviceUsage, 0)
		}
		return writeResult(stderr, fmt.Sprintf("tuna: %v\n%s", err, serviceUsage), usageExitCode)
	}
	if fs.NArg() != 0 || *port < 1 || *port > 65535 || *maxAge <= 0 || *maxCount < 1 ||
		*launchTimeout < time.Millisecond || *launchTimeout%time.Millisecond != 0 ||
		*cancelAckTimeout < time.Millisecond || *cancelAckTimeout%time.Millisecond != 0 ||
		*cancelCompletionTimeout < time.Millisecond || *cancelCompletionTimeout%time.Millisecond != 0 {
		return writeResult(stderr, "tuna: invalid service arguments\n"+serviceUsage, usageExitCode)
	}
	return l.service(service.Options{
		Port:                    *port,
		DataDir:                 *dataDir,
		Capacity:                capacity,
		BackfillLimit:           backfillLimit,
		ServiceVersion:          version,
		Log:                     stderr,
		EventMaxAge:             *maxAge,
		EventMaxCount:           *maxCount,
		LaunchTimeout:           *launchTimeout,
		CancelAckTimeout:        *cancelAckTimeout,
		CancelCompletionTimeout: *cancelCompletionTimeout,
	})
}

var valued = map[string]bool{"--port": true, "--weight": true, "--priority": true, "--worklog": true, "--repository": true, "--retries": true}

// option reads a valued option written as "--name value" or "--name=value".
// Each option may be given once.
func option(args []string, i *int, seen map[string]bool) (name, value string, ok bool, err error) {
	name, value, inline := strings.Cut(args[*i], "=")
	if !valued[name] {
		return "", "", false, nil
	}
	if !inline {
		*i++
		if *i == len(args) {
			return "", "", false, fmt.Errorf("%s requires a value", name)
		}
		value = args[*i]
	}
	if seen[name] {
		return "", "", false, fmt.Errorf("%s may be given once", name)
	}
	seen[name] = true
	return name, value, true, nil
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("--port must be between 1 and 65535")
	}
	return port, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
