// logctl is the operator CLI: query/tail/verify/export over gRPC, plus an
// MQTT simulator that emits a realistic sanitization job (also serving as
// the reference for the C++ producer implementation).
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "logctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf(`usage: logctl <command> [flags]

commands:
  query    fetch historical entries (hot + cold, merged)
  tail     follow live entries
  verify   re-verify signatures and hash chain for a device
  export   export a signed audit report for a sanitization job (trace id)
  stats    per-device ingest statistics
  sim      publish a simulated sanitization job over MQTT/TLS`)
	}
	switch args[0] {
	case "query":
		return cmdQuery(args[1:])
	case "tail":
		return cmdTail(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "stats":
		return cmdStats(args[1:])
	case "sim":
		return cmdSim(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
