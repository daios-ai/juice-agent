package channels

import "testing"

// The generic service driver must satisfy the Channel contract every intake
// surface shares, and Kernel must remain usable wherever a TaskSource is needed.
var (
	_ Channel    = (*Service)(nil)
	_ TaskSource = Kernel(nil)
)

func TestServiceNameIsItsConfiguredName(t *testing.T) {
	s := NewService(nil, ServiceConfig{Name: "svc", DataDir: t.TempDir()})
	if s.Name() != "svc" {
		t.Fatalf("Name() = %q, want svc", s.Name())
	}
}
