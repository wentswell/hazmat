package sandbox

import "golang.org/x/sys/unix"

// limit sets the resource limits of the child. It runs just after the start:
// Go has no way to set them before the exec without a helper program. The
// child has done nothing with the file in that time.
func limit(pid int, lim Limits) {
	set := func(resource int, v int64) {
		if v > 0 {
			_ = unix.Prlimit(pid, resource, &unix.Rlimit{Cur: uint64(v), Max: uint64(v)}, nil)
		}
	}
	set(unix.RLIMIT_AS, lim.MemoryBytes)
	set(unix.RLIMIT_FSIZE, lim.FileBytes)
	// CPU time is a second stop behind the wall-clock timeout: all threads
	// together can use more CPU seconds than wall seconds.
	set(unix.RLIMIT_CPU, int64(lim.Timeout.Seconds())*4+1)
	set(unix.RLIMIT_CORE, 0)
}
