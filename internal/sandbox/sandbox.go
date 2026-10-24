// Package sandbox runs the decoder as a child process with as little as the
// worker can give it: no environment, its own process group, a time limit, and
// limits on memory, CPU time, and file size.
//
// It is not a security boundary. The child has the user of the worker, so the
// role of the function is the real limit of what an exploited decoder can do.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

type Limits struct {
	Timeout     time.Duration
	MemoryBytes int64 // address space of the child
	FileBytes   int64 // largest file the child can write
	StdoutBytes int   // more output than this stops the child
}

type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
	// TimedOut and Signaled say the child did not end by itself. After
	// either, the worker does not trust this execution environment.
	TimedOut bool
	Signaled bool
}

// Abnormal reports an end that a correct decoder on a correct file never has.
func (r Result) Abnormal() bool { return r.TimedOut || r.Signaled }

const stderrBytes = 64 << 10

// Run starts name with args in dir and waits. The error is non-nil only when
// the child could not start; every way the child can end is in Result.
func Run(ctx context.Context, lim Limits, dir, name string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, lim.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// The child gets no variable of the worker: no credentials, no region.
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// On a timeout, kill the group, so a process the child started dies too.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	stdout := &capped{max: lim.StdoutBytes, stop: cancel}
	stderr := &capped{max: stderrBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	limit(cmd.Process.Pid, lim)
	err := cmd.Wait()
	// Nothing of the group may outlive the call.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	res := Result{Stdout: stdout.buf.Bytes(), Stderr: stderr.buf.Bytes()}
	res.TimedOut = ctx.Err() != nil
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signaled = true
		}
	default:
		// A pipe error after the child ended. Treat it as a failed run.
		res.ExitCode = -1
	}
	return res, nil
}

// capped keeps the first max bytes. With a stop function, it stops the child
// when there is more: a child that prints without end is not a correct one.
type capped struct {
	buf  bytes.Buffer
	max  int
	stop func()
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room >= len(p) {
		c.buf.Write(p)
	} else {
		if room > 0 {
			c.buf.Write(p[:room])
		}
		if c.stop != nil {
			c.stop()
		}
	}
	return len(p), nil
}
