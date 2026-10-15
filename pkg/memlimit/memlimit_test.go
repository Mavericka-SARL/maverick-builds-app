package memlimit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContainerLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		name  string
		paths []string
		want  int64
		ok    bool
	}{
		{"cgroup v2 limit", []string{write("v2", "536870912\n")}, 536870912, true},
		{"cgroup v2 without a limit", []string{write("v2max", "max\n")}, 0, false},
		{"cgroup v1 without a limit", []string{write("v1", "9223372036854771712\n")}, 0, false},
		{"v2 missing, v1 limit", []string{filepath.Join(dir, "absent"), write("v1lim", "1073741824")}, 1073741824, true},
		{"outside a container", []string{filepath.Join(dir, "absent")}, 0, false},
	} {
		got, ok := containerLimit(c.paths)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %d, %v; want %d, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestAnOperatorsGOMEMLIMITStands(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "300MiB")
	if _, ok := Set(); ok {
		t.Error("Set replaced the limit GOMEMLIMIT gave")
	}
}
