//go:build !linux

package sandbox

// limit does nothing outside Linux. The timeout and the process group still
// apply. The worker runs on Linux; other systems are for development.
func limit(int, Limits) {}
