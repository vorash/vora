// Package cli implements the vora command-line interface.
package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/vorash/vora/internal/client"
	"github.com/vorash/vora/internal/config"
	"github.com/vorash/vora/internal/version"
)

type app struct {
	socket string
	api    string
	apiKey string
	json   bool
}

// exitError carries an exit code that Execute should propagate.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	a := &app{}
	root := &cobra.Command{
		Use:           "vora",
		Short:         "Secure execution infrastructure for autonomous agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&a.socket, "socket", defaultSocket(), "runner control socket")
	root.PersistentFlags().StringVar(&a.api, "api", "", "vora-api base URL (e.g. http://127.0.0.1:9300); uses the remote HTTP API instead of the local socket")
	root.PersistentFlags().StringVar(&a.apiKey, "api-key", "", "API key for --api")
	root.PersistentFlags().BoolVar(&a.json, "json", false, "output JSON")

	root.AddCommand(
		a.sandboxCmd(),
		a.execCmd(),
		a.cpCmd(),
		a.logsCmd(),
		a.statsCmd(),
		a.runnersCmd(),
		a.keysCmd(),
		a.secretsCmd(),
		a.versionCmd(),
	)

	if err := root.Execute(); err != nil {
		var ee exitError
		if asExit(err, &ee) {
			return ee.code
		}
		fmt.Fprintln(os.Stderr, "vora:", err)
		return 1
	}
	return 0
}

func asExit(err error, target *exitError) bool {
	ee, ok := err.(exitError)
	if ok {
		*target = ee
	}
	return ok
}

func defaultSocket() string {
	cfg, err := config.Default()
	if err != nil {
		return "/var/run/vora/runner.sock"
	}
	return cfg.SocketPath
}

// dial connects to the runner or to vora-api. When --api is set the remote
// HTTP client is used; otherwise the local runner socket.
func (a *app) dial() (client.API, context.Context, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if a.api != "" {
		c, err := client.DialAPI(a.api, a.apiKey)
		if err != nil {
			cancel()
			return nil, nil, nil, err
		}
		return c, ctx, cancel, nil
	}
	c, err := client.Dial(a.socket)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return c, ctx, cancel, nil
}

// jsonOut prints a protobuf message as JSON when --json is set.
func (a *app) jsonOut(msg proto.Message) error {
	data, err := protojson.MarshalOptions{Indent: "  "}.Marshal(msg)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the vora version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.json {
				fmt.Printf("{\"version\":%q}\n", version.String())
				return nil
			}
			fmt.Println("vora", version.String())
			return nil
		},
	}
}
