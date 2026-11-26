package main

import (
	"io"
	"os"
)

const (
	usageExitCode    = 64
	softwareExitCode = 70
	usage            = "Usage: tuna [runner options] -- <command> [arguments...]\n" +
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
			return writeResult(stderr, "tuna: runner mode is not implemented\n", softwareExitCode)
		}
	}
	return writeResult(stderr, usage, usageExitCode)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
