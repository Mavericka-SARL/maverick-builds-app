// Package memlimit tells the Go runtime the memory limit of the container
// it runs in.
//
// The runtime does not read a container's limit: its garbage collector let
// the heap grow to twice what was live before collecting, so a gateway whose
// recalculation held 300 MB was killed at its 512 MiB limit although what it
// held fitted (both production gateways were OOM-killed five times in ten
// minutes while a 618-metric model recalculated). With the limit known, the
// collector works harder as the heap nears it instead.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// share is the part of the container's limit the heap may use; the rest is
// for the runtime, goroutine stacks and memory the heap does not count.
const share = 0.9

// files are where the container's limit is: cgroup v2, then v1.
var files = []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"}

// Set sets the runtime's memory limit to 90% of the container's and returns
// it, unless GOMEMLIMIT is set (the operator's choice stands) or there is no
// limit to read (a process outside a container, or one without a limit).
func Set() (bytes int64, ok bool) {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0, false
	}
	limit, ok := containerLimit(files)
	if !ok {
		return 0, false
	}
	bytes = int64(float64(limit) * share)
	debug.SetMemoryLimit(bytes)
	return bytes, true
}

// containerLimit reads the first of paths that holds a limit. cgroup v2
// writes "max" for none, v1 a number near the largest int64.
func containerLimit(paths []string) (int64, bool) {
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(raw))
		if v == "max" {
			return 0, false
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n >= 1<<60 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
