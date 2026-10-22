package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CommandRunner runs the commands of a deployment.
//
// Every command writes to the log of the deployment, so a failure can be read
// back afterwards. The output of a failed command is kept with the error: a
// command that exits with a status code rarely says why on its own.
type CommandRunner struct {
	// Env is the environment the commands are run in. It replaces the
	// environment of the service, so a command does not accidentally inherit
	// anything it should not have.
	Env []string
	// Log receives the output of every command.
	Log io.Writer

	mu  sync.Mutex
	buf bytes.Buffer
}

// Result is the outcome of a command.
type Result struct {
	// Output is everything the command wrote.
	Output string
	// ExitCode is the status the command exited with.
	ExitCode int
}

// Run runs a command and waits for it to finish.
//
// The command is given as an argument list, not as a shell string, so nothing
// in it is interpreted by a shell.
func (c *CommandRunner) Run(ctx context.Context, name string, argv []string, dir string) (*Result, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("the %s command is empty", name)
	}

	c.logf("\n=== %s: %s\n", name, strings.Join(argv, " "))

	commands := make([]string, len(argv))
	for i, arg := range argv {
		commands[i] = expandHome(arg, c.home())
	}

	cmd := exec.CommandContext(ctx, commands[0], commands[1:]...)
	cmd.Dir = dir
	cmd.Env = c.Env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err := cmd.Run()

	result := &Result{Output: out.String()}

	if c.Log != nil {
		c.Log.Write(out.Bytes())
	}

	if err != nil {
		result.ExitCode = -1

		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			// The command never ran: it could not be found, or it could not
			// be started. Say so, rather than reporting a status code that
			// says nothing.
			return result, fmt.Errorf("the %s command could not be run: %w", name, err)
		}

		tail := tail(result.Output, 20)
		if tail == "" {
			return result, fmt.Errorf("the %s command failed with exit code %d", name, result.ExitCode)
		}

		return result, fmt.Errorf("the %s command failed with exit code %d: %s", name, result.ExitCode, tail)
	}

	return result, nil
}

// home returns the home directory the commands are run with.
func (c *CommandRunner) home() string {
	for _, entry := range c.Env {
		if strings.HasPrefix(entry, "HOME=") {
			return strings.TrimPrefix(entry, "HOME=")
		}
	}

	return ""
}

// expandHome replaces a leading "~" with the home directory, the way a shell
// would. The commands are not run through a shell, so nothing else expands it
// for them.
func expandHome(path string, home string) string {
	if home == "" {
		return path
	}

	if path == "~" {
		return home
	}

	if strings.HasPrefix(path, "~/") {
		return home + "/" + path[2:]
	}

	return path
}

// logf writes a line to the log.
func (c *CommandRunner) logf(format string, args ...any) {
	if c.Log == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	fmt.Fprintf(c.Log, format, args...)
}

// tail returns the last n lines of a block of output.
func tail(output string, n int) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}

// Environment builds the environment for the commands of a deployment.
//
// The PATH is fixed so that the commands find the same tools no matter how
// the service was started, and the home directory is the one of the user the
// applications belong to.
func Environment(home string, extra ...string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + home + "/.local/share/pnpm:" + home + "/.bun/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
	}

	return append(env, extra...)
}

// OpenLog opens the log file of a deployment.
func OpenLog(path string) (*os.File, error) {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return nil, err
	}

	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// dirOf returns the directory a path is in.
func dirOf(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "."
	}
	return path[:idx]
}
