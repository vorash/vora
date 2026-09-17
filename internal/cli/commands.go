package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/vorash/vora/api/proto/v1"
	"github.com/vorash/vora/internal/client"
)

func (a *app) sandboxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sandbox", Short: "Manage sandboxes"}
	cmd.AddCommand(a.sandboxCreateCmd(), a.sandboxListCmd(), a.sandboxInspectCmd(), a.sandboxDestroyCmd())
	return cmd
}

func (a *app) sandboxCreateCmd() *cobra.Command {
	var (
		image, memory, cpu, network, ttl string
		backend                          string
		pids                             int64
		env                              []string
		tenant, agent                    string
		allowedDomains                   []string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create and start a sandbox",
		RunE: func(cmd *cobra.Command, _ []string) error {
			memBytes, err := parseMemory(memory)
			if err != nil {
				return err
			}
			cpuMillis, err := parseCPU(cpu)
			if err != nil {
				return err
			}
			mode, err := parseNetwork(network)
			if err != nil {
				return err
			}
			ttlDur, err := parseDuration(ttl)
			if err != nil {
				return err
			}

			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			spec := &v1.SandboxSpec{
				Image:      image,
				Backend:    backend,
				TenantId:   tenant,
				AgentId:    agent,
				Resources:  &v1.ResourceLimits{CpuMillis: cpuMillis, MemoryBytes: memBytes, Pids: pids},
				Network:    &v1.NetworkPolicy{Mode: mode, AllowedDomains: allowedDomains},
				Env:        env,
				TtlSeconds: int64(ttlDur / time.Second),
			}
			sb, err := c.CreateSandbox(ctx, spec)
			if err != nil {
				return err
			}
			started, err := c.StartSandbox(ctx, sb.GetId())
			if err != nil {
				return fmt.Errorf("sandbox %s created but failed to start: %w", sb.GetId(), err)
			}
			if a.json {
				return a.jsonOut(started)
			}
			fmt.Printf("Sandbox created: %s\n", sb.GetId())
			return nil
		},
	}
	cmd.Flags().StringVar(&image, "image", "vora/base", "rootfs image (e.g. python, node, vora/base); tags must match the cache exactly")
	cmd.Flags().StringVar(&backend, "backend", "", "runtime backend: gvisor or bwrap (default: auto)")
	cmd.Flags().StringVar(&memory, "memory", "512m", "memory limit (e.g. 512m, 1Gi)")
	cmd.Flags().StringVar(&cpu, "cpu", "1", "CPU limit (e.g. 1, 500m)")
	cmd.Flags().Int64Var(&pids, "pids", 128, "process limit")
	cmd.Flags().StringVar(&network, "network", "none", "network mode: none or restricted")
	cmd.Flags().StringArrayVar(&allowedDomains, "allow-domain", nil, "egress allowlist domain for --network restricted (repeatable, e.g. example.com)")
	cmd.Flags().StringVar(&ttl, "ttl", "30m", "sandbox time to live")
	cmd.Flags().StringArrayVar(&env, "env", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().StringVar(&tenant, "tenant", "", "tenant id")
	cmd.Flags().StringVar(&agent, "agent", "", "agent id")
	return cmd
}

func (a *app) sandboxListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List sandboxes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			sandboxes, err := c.ListSandboxes(ctx, all)
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOut(&v1.ListSandboxesResponse{Sandboxes: sandboxes})
			}
			if len(sandboxes) == 0 {
				fmt.Println("no sandboxes")
				return nil
			}
			fmt.Printf("%-30s %-10s %-14s %s\n", "ID", "STATE", "IMAGE", "CREATED")
			for _, sb := range sandboxes {
				created := time.UnixMilli(sb.GetCreatedAtUnixMs()).Format(time.RFC3339)
				fmt.Printf("%-30s %-10s %-14s %s\n", sb.GetId(), shortState(sb.GetState()), sb.GetSpec().GetImage(), created)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include destroyed sandboxes")
	return cmd
}

func (a *app) sandboxInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <sandbox-id>",
		Short: "Show a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			sb, err := c.GetSandbox(ctx, args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOut(sb)
			}
			fmt.Printf("id:         %s\n", sb.GetId())
			fmt.Printf("state:      %s\n", shortState(sb.GetState()))
			fmt.Printf("image:      %s\n", sb.GetSpec().GetImage())
			if be := sb.GetSpec().GetBackend(); be != "" {
				fmt.Printf("backend:    %s\n", be)
			}
			fmt.Printf("cpu:        %dm\n", sb.GetSpec().GetResources().GetCpuMillis())
			fmt.Printf("memory:     %d bytes\n", sb.GetSpec().GetResources().GetMemoryBytes())
			fmt.Printf("pids:       %d\n", sb.GetSpec().GetResources().GetPids())
			fmt.Printf("network:    %s\n", shortNetwork(sb.GetSpec().GetNetwork().GetMode()))
			fmt.Printf("runner:     %s\n", sb.GetRunnerId())
			fmt.Printf("created:    %s\n", time.UnixMilli(sb.GetCreatedAtUnixMs()).Format(time.RFC3339))
			fmt.Printf("updated:    %s\n", time.UnixMilli(sb.GetUpdatedAtUnixMs()).Format(time.RFC3339))
			if rootfs := sb.GetImageRootfs(); rootfs != "" {
				fmt.Printf("rootfs:     %s\n", rootfs)
			}
			return nil
		},
	}
}

func (a *app) sandboxDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "destroy <sandbox-id>",
		Aliases: []string{"rm"},
		Short:   "Destroy a sandbox",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()
			if err := c.DestroySandbox(ctx, args[0]); err != nil {
				return err
			}
			fmt.Printf("Destroyed %s\n", args[0])
			return nil
		},
	}
}

func (a *app) execCmd() *cobra.Command {
	var cwd string
	var env []string
	cmd := &cobra.Command{
		Use:                   "exec <sandbox-id> -- <command> [args...]",
		Short:                 "Run a command in a sandbox",
		Args:                  cobra.MinimumNArgs(2),
		DisableFlagsInUseLine: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, command, rest := args[0], args[1], args[2:]

			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			code, err := a.runExec(ctx, c, id, &v1.ExecRequest{
				Command: command,
				Args:    rest,
				Cwd:     cwd,
				Env:     env,
			}, os.Stdout, os.Stderr)
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{code: int(code)}
			}
			return nil
		},
	}
	// The command and its own flags follow `--`; pflag consumes the separator.
	cmd.Flags().StringVar(&cwd, "workdir", "/workspace", "working directory inside the sandbox")
	cmd.Flags().StringArrayVar(&env, "env", nil, "environment variable KEY=VALUE (repeatable)")
	return cmd
}

// runExec streams a command's output to stdout/stderr and returns its exit code.
func (a *app) runExec(ctx context.Context, c *client.Client, id string, req *v1.ExecRequest, stdout, stderr io.Writer) (int32, error) {
	stream, err := c.Exec(ctx, &v1.ExecSandboxRequest{SandboxId: id, Exec: req})
	if err != nil {
		return 0, err
	}
	var exit int32
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return exit, nil
		}
		if err != nil {
			return exit, err
		}
		if b := chunk.GetStdout(); len(b) > 0 {
			_, _ = stdout.Write(b)
		}
		if b := chunk.GetStderr(); len(b) > 0 {
			_, _ = stderr.Write(b)
		}
		if e := chunk.GetExit(); e != nil {
			exit = e.GetExitCode()
		}
	}
}

func (a *app) cpCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cp <local> <sandbox-id>:<path>  |  vora cp <sandbox-id>:<path> <local>",
		Short: "Copy files into or out of a sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			srcID, srcPath, srcIsSandbox := splitEndpoint(args[0])
			dstID, dstPath, dstIsSandbox := splitEndpoint(args[1])
			if srcIsSandbox == dstIsSandbox {
				return fmt.Errorf("exactly one of the two paths must be a sandbox path (<id>:<path>)")
			}

			if srcIsSandbox {
				// sandbox -> local
				var buf bytes.Buffer
				if _, err := a.runExec(ctx, c, srcID, &v1.ExecRequest{Command: "cat", Args: []string{srcPath}, Cwd: "/"}, &buf, os.Stderr); err != nil {
					return err
				}
				return os.WriteFile(dstPath, buf.Bytes(), 0o644)
			}
			// local -> sandbox
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			_, err = a.runExec(ctx, c, dstID, &v1.ExecRequest{
				Command: "sh",
				Args:    []string{"-c", `cat > "$1"`, "vora", dstPath},
				Cwd:     "/",
				Stdin:   data,
			}, io.Discard, os.Stderr)
			return err
		},
	}
}

// splitEndpoint parses "<sandbox-id>:<path>" or a local path.
func splitEndpoint(s string) (id, path string, isSandbox bool) {
	if strings.Contains(s, ":") && !strings.HasPrefix(s, "/") && !strings.HasPrefix(s, "./") && !strings.HasPrefix(s, "~") {
		id, path, _ = strings.Cut(s, ":")
		return id, path, true
	}
	return "", s, false
}

func (a *app) logsCmd() *cobra.Command {
	var showExecs bool
	cmd := &cobra.Command{
		Use:   "logs <sandbox-id>",
		Short: "Show a sandbox's lifecycle events or execution records",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			if showExecs {
				records, err := c.Execs(ctx, args[0])
				if err != nil {
					return err
				}
				if a.json {
					return a.jsonOut(&v1.GetSandboxExecsResponse{Execs: records})
				}
				for _, e := range records {
					ts := time.UnixMilli(e.GetStartedAtUnixMs()).Format(time.RFC3339)
					fmt.Printf("%s  %s %s  exit=%d reason=%s stdout=%dB stderr=%dB\n",
						ts, e.GetCommand(), strings.Join(e.GetArgs(), " "),
						e.GetExitCode(), shortReason(e.GetReason()), e.GetStdoutBytes(), e.GetStderrBytes())
				}
				return nil
			}

			events, err := c.Events(ctx, args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOut(&v1.GetSandboxEventsResponse{Events: events})
			}
			for _, e := range events {
				ts := time.UnixMilli(e.GetTimestampUnixMs()).Format(time.RFC3339)
				reason := e.GetReason()
				if reason != "" {
					reason = " (" + reason + ")"
				}
				fmt.Printf("%s  %s -> %s%s\n", ts, e.GetFrom(), e.GetTo(), reason)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showExecs, "execs", false, "show execution records instead of lifecycle events")
	return cmd
}

func shortReason(r v1.ExitReason) string {
	return strings.TrimPrefix(r.String(), "EXIT_REASON_")
}

func (a *app) statsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats <sandbox-id>",
		Short: "Show resource usage for a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ctx, cancel, err := a.dial()
			if err != nil {
				return err
			}
			defer cancel()
			defer c.Close()

			st, err := c.StatsSandbox(ctx, args[0])
			if err != nil {
				return err
			}
			if a.json {
				return a.jsonOut(st)
			}
			fmt.Printf("memory:    %d bytes\n", st.GetMemoryBytes())
			fmt.Printf("peak:      %d bytes\n", st.GetMemoryPeakBytes())
			fmt.Printf("cpu:       %d millis\n", st.GetCpuMillis())
			fmt.Printf("pids:      %d\n", st.GetPids())
			fmt.Printf("oom kills: %d\n", st.GetOomKills())
			fmt.Printf("pid limit: %d\n", st.GetPidsLimitHits())
			return nil
		},
	}
}

func shortState(s v1.SandboxState) string {
	return strings.TrimPrefix(s.String(), "SANDBOX_STATE_")
}

func shortNetwork(m v1.NetworkMode) string {
	return strings.TrimPrefix(m.String(), "NETWORK_")
}
