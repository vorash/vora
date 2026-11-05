// Command forgedreset is what a workload can do over the control socket: call the
// agent's Reset itself, with any manifest it likes. It runs only inside a sandbox, as
// part of TestForgedResetGainsNothing.
//
// Usage: forgedreset <manifest file> <timeout>
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/vorash/vora/internal/agentproto"
)

func main() {
	manifest, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("read manifest:", err)
		os.Exit(2)
	}
	timeout, err := time.ParseDuration(os.Args[2])
	if err != nil {
		fmt.Println("parse timeout:", err)
		os.Exit(2)
	}
	client, err := agentproto.Dial("/vora/control.sock")
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(2)
	}
	defer func() { _ = client.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	removed, err := client.Reset(ctx, manifest)
	fmt.Println("removed", removed, "error", err)
}
