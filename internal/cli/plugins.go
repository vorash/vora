package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// Plugin dispatch follows the git/kubectl convention: an otherwise-unknown
// subcommand `vora <name>` runs `vora-<name>` from $PATH. See
// docs/architecture/threat-model.md for why this adds no new execution path.
const (
	// pluginPrefix is prepended to the subcommand name to form the binary name.
	pluginPrefix = "vora-"
	// disablePluginsEnv disables dispatch entirely when set to a truthy value.
	disablePluginsEnv = "VORA_DISABLE_PLUGINS"
	// pluginExitNotFound is used when a valid plugin name has no binary. It is
	// the conventional "command not found" code, distinct from a plugin's own
	// exit codes.
	pluginExitNotFound = 127
	// groupCore and groupPlugin are the help sections for built-ins and plugins.
	groupCore   = "core"
	groupPlugin = "plugin"
)

// pluginNamePattern is deliberately strict: a name can never contain a path
// separator, `.`, leading `-`, uppercase or whitespace, so it cannot escape the
// `vora-` prefix or be interpreted by a shell.
var pluginNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// companionCommands names Vora's own binaries that sit next to `vora` on $PATH.
// They are excluded from dispatch: `vora api`/`vora runner` would launch a
// daemon and `vora init` reaches the in-sandbox agent, which is surprising for
// names that read like CLI nouns. Run the binaries directly instead.
var companionCommands = map[string]bool{
	"api":    true, // vora-api
	"runner": true, // vora-runner
	"init":   true, // vora-init
}

type plugin struct {
	name string
	path string
}

// validPluginName reports whether name is a syntactically valid plugin
// subcommand name (≤64 bytes, `pluginNamePattern`).
func validPluginName(name string) bool {
	return len(name) >= 1 && len(name) <= 64 && pluginNamePattern.MatchString(name)
}

// pluginsDisabled reports whether dispatch is turned off by the environment.
func pluginsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(disablePluginsEnv))) {
	case "", "0", "false", "no":
		return false
	}
	return true
}

// discoverPlugins returns the valid `vora-<name>` binaries on $PATH that do not
// collide with a built-in, sorted by name. It never fails: discovery must not
// break the CLI, so an unreadable $PATH entry or a vanished binary is skipped.
func discoverPlugins(builtins map[string]bool) []plugin {
	candidates := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutPrefix(e.Name(), pluginPrefix)
			if !ok || builtins[name] || companionCommands[name] || !validPluginName(name) {
				continue
			}
			candidates[name] = true
		}
	}
	out := make([]plugin, 0, len(candidates))
	for name := range candidates {
		// LookPath resolves $PATH order and the executable bit for us, and
		// rejects directories.
		path, err := exec.LookPath(pluginPrefix + name)
		if err != nil {
			continue
		}
		out = append(out, plugin{name: name, path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// loadPlugins attaches a command for every discovered plugin. Built-ins are
// never shadowed: they are claimed first and excluded from discovery.
//
// Command groups are only introduced when a plugin actually exists, so the help
// output for a plain `vora` install is unchanged.
func loadPlugins(root *cobra.Command) {
	if pluginsDisabled() {
		return
	}
	plugins := discoverPlugins(builtinNames(root))
	if len(plugins) == 0 {
		return
	}
	root.AddGroup(
		&cobra.Group{ID: groupCore, Title: "Available Commands:"},
		&cobra.Group{ID: groupPlugin, Title: "External plugins:"},
	)
	root.SetHelpCommandGroupID(groupCore)
	root.SetCompletionCommandGroupID(groupCore)
	for _, c := range root.Commands() {
		c.GroupID = groupCore
	}
	for _, p := range plugins {
		root.AddCommand(pluginCommand(p))
	}
}

// builtinNames is the set of subcommands vora owns. Cobra's default help and
// completion commands are not present until Execute, so they are added here to
// keep a plugin from claiming them.
func builtinNames(root *cobra.Command) map[string]bool {
	names := map[string]bool{"help": true, "completion": true}
	for _, c := range root.Commands() {
		names[c.Name()] = true
	}
	return names
}

func pluginCommand(p plugin) *cobra.Command {
	return &cobra.Command{
		Use:                   p.name,
		Short:                 "External plugin (" + pluginPrefix + p.name + ")",
		GroupID:               groupPlugin,
		DisableFlagParsing:    true,
		DisableFlagsInUseLine: true,
		SilenceUsage:          true,
		SilenceErrors:         true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlugin(cmd, p.path, args)
		},
	}
}

// runPlugin execs the plugin with args forwarded verbatim, stdio attached
// directly, and the plugin's exit code propagated.
func runPlugin(cmd *cobra.Command, path string, args []string) error {
	c := exec.Command(path, args...)
	c.Stdin = cmd.InOrStdin()
	c.Stdout = cmd.OutOrStdout()
	c.Stderr = cmd.ErrOrStderr()
	c.Env = os.Environ()
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitError{code: pluginExitCode(ee)}
		}
		return fmt.Errorf("running plugin %s: %w", path, err)
	}
	return nil
}

// pluginExitCode maps a finished plugin process to an exit code. A process
// killed by a signal has ExitCode -1; report the shell convention 128+signal so
// it surfaces as a failure rather than success.
func pluginExitCode(ee *exec.ExitError) int {
	if code := ee.ExitCode(); code >= 0 {
		return code
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return 1
}

// unknownCommand handles a subcommand that matched no built-in or plugin.
func unknownCommand(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	name := args[0]
	if !validPluginName(name) {
		return fmt.Errorf("unknown command %q for %q", name, cmd.CommandPath())
	}
	// Vora's own companion binaries are reserved: report a normal unknown-command
	// error rather than claiming no `vora-<name>` exists.
	if companionCommands[name] {
		return fmt.Errorf("unknown command %q for %q", name, cmd.CommandPath())
	}
	// A literal `--` stops cobra's lookup, so the plugin never became the
	// executing command. Run it explicitly to keep passthrough faithful.
	if sub, _, err := cmd.Root().Find([]string{name}); err == nil && sub != cmd && sub.GroupID == groupPlugin {
		return sub.RunE(sub, args[1:])
	}
	if pluginsDisabled() {
		return exitError{
			code: pluginExitNotFound,
			msg: fmt.Sprintf("unknown command %q for %q\nplugins are disabled (%s is set)",
				name, cmd.CommandPath(), disablePluginsEnv),
		}
	}
	return exitError{code: pluginExitNotFound, msg: pluginNotFoundMessage(name)}
}

func pluginNotFoundMessage(name string) string {
	return fmt.Sprintf("'%s' is not a vora command and no '%s%s' was found on PATH\nrun 'vora --help' for the list of commands",
		name, pluginPrefix, name)
}
