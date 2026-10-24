package sandbox

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

var lim = Limits{Timeout: 5 * time.Second, StdoutBytes: 1 << 10}

func TestRun(t *testing.T) {
	os.Setenv("HAZMAT_TEST_SECRET", "do-not-leak")
	defer os.Unsetenv("HAZMAT_TEST_SECRET")
	dir := t.TempDir()

	res, err := Run(context.Background(), lim, dir, "/bin/sh", "-c", "pwd; env; echo oops >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || res.Abnormal() {
		t.Errorf("exit: %+v", res)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, dir) {
		t.Errorf("the child did not run in %s: %q", dir, out)
	}
	// The child gets none of the variables of the worker.
	if strings.Contains(out, "do-not-leak") || strings.Contains(out, "HOME=") {
		t.Errorf("the child has the environment of the worker: %q", out)
	}
	if strings.TrimSpace(string(res.Stderr)) != "oops" {
		t.Errorf("stderr: %q", res.Stderr)
	}
}

func TestTimeoutKillsTheGroup(t *testing.T) {
	short := Limits{Timeout: 300 * time.Millisecond, StdoutBytes: 1 << 10}
	start := time.Now()
	// The shell starts a second process. Both must die.
	res, err := Run(context.Background(), short, t.TempDir(), "/bin/sh", "-c", "sleep 30 & echo $!; wait")
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.Abnormal() {
		t.Errorf("want a timeout: %+v", res)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("the run took %s", took)
	}
	pid := strings.TrimSpace(string(res.Stdout))
	time.Sleep(100 * time.Millisecond)
	if err := shell("kill", "-0", pid); err == nil {
		t.Errorf("the second process %s is alive after the timeout", pid)
	}
}

func TestSignalIsAbnormal(t *testing.T) {
	res, err := Run(context.Background(), lim, t.TempDir(), "/bin/sh", "-c", "kill -SEGV $$")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Signaled || res.TimedOut || !res.Abnormal() {
		t.Errorf("want a signal: %+v", res)
	}
}

func TestTooMuchOutputStopsTheChild(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), lim, t.TempDir(), "/bin/sh", "-c", "while :; do echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; done")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 1<<10 || !res.Abnormal() || time.Since(start) > 3*time.Second {
		t.Errorf("stdout %d bytes, %+v, %s", len(res.Stdout), res, time.Since(start))
	}
	// Nothing timed out and the child sent itself no signal.
	if !res.Overflowed || res.TimedOut || res.Signaled {
		t.Errorf("want an overflow and nothing else: %+v", res)
	}
	// Exactly the limit is not too much.
	res, err = Run(context.Background(), lim, t.TempDir(), "/bin/sh", "-c", "head -c 1024 /dev/zero")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != 1<<10 || res.Overflowed || res.Abnormal() || res.ExitCode != 0 {
		t.Errorf("1 KiB of 1 KiB: %d bytes, %+v", len(res.Stdout), res)
	}
}

// A lot on stderr is cut, not a stop: stderr is for the frame lines.
func TestStderrIsCutNotStopped(t *testing.T) {
	res, err := Run(context.Background(), lim, t.TempDir(), "/bin/sh", "-c", "head -c 200000 /dev/zero >&2; echo done")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stderr) != stderrBytes || res.Abnormal() || strings.TrimSpace(string(res.Stdout)) != "done" {
		t.Errorf("stderr %d bytes, %+v", len(res.Stderr), res)
	}
}

func TestMissingProgram(t *testing.T) {
	if _, err := Run(context.Background(), lim, t.TempDir(), "/no/such/program"); err == nil {
		t.Error("a missing program gave no error")
	}
}

func shell(name string, args ...string) error {
	res, err := Run(context.Background(), lim, ".", "/bin/sh", "-c", name+" "+strings.Join(args, " "))
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return os.ErrNotExist
	}
	return nil
}
