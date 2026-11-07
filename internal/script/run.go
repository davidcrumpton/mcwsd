package script

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// hostResult is the normalized output of running a host command.
type hostResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runHostCommand executes argv on the host with optional stdin, enforcing the
// allowed-binary gate when binaries is non-empty. A non-zero exit is returned
// in the result (exitCode), not as an error; only a local failure (disallowed
// or missing binary, context cancellation) is an error.
func runHostCommand(ctx context.Context, argv []string, stdin io.Reader, binaries map[string]bool) (hostResult, error) {
	if len(argv) == 0 {
		return hostResult{}, fmt.Errorf("command argv must not be empty")
	}
	if len(binaries) > 0 && !binaries[argv[0]] {
		return hostResult{}, fmt.Errorf("binary %q is not in the plugin's allowed list", argv[0])
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return hostResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: exitErr.ExitCode()}, nil
		}
		if ctx.Err() != nil {
			return hostResult{}, ctx.Err()
		}
		return hostResult{}, fmt.Errorf("run %s: %w", redactCommand(argv), err)
	}

	_ = durationMs
	return hostResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}, nil
}
