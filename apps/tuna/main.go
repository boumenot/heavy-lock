package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

const (
	usageExitCode    = 64
	softwareExitCode = 70
	usage            = "Usage: tuna --port <port> [runner options] -- <command> [arguments...]\n" +
		"       tuna service\n\n" +
		"The v2 runner and service are not implemented yet.\n"
)

func writeResult(w io.Writer, message string, code int) int {
	if _, err := io.WriteString(w, message); err != nil {
		return softwareExitCode
	}
	return code
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return writeResult(stdout, usage, 0)
	}
	if len(args) > 0 && args[0] == "service" {
		return writeResult(stderr, "tuna: service mode is not implemented\n", softwareExitCode)
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
