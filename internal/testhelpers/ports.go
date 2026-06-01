package testhelpers

import (
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
)

// freeHostPort grabs an OS-assigned free TCP port and immediately releases it
// so a container can be pinned to it. There's a small TOCTOU window between the
// release and the container binding the port — acceptable for the handful of
// chaos tests (StartPostgresC/StartClickHouseC) that need a stable host port.
func freeHostPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// pinHostPort returns a customizer that binds the container's portSpec (e.g.
// "5432/tcp") to a fixed host port. testcontainers' default ephemeral mapping
// is reassigned by Docker across a Stop/Start cycle; a pinned host port is part
// of the container's HostConfig and survives the restart, so a pool opened
// against it reconnects on the same address. That's what lets the chaos tests
// assert recovery rather than just failure.
func pinHostPort(t *testing.T, portSpec string, hostPort int) testcontainers.CustomizeRequestOption {
	t.Helper()
	p, err := network.ParsePort(portSpec)
	if err != nil {
		t.Fatalf("parse port %q: %v", portSpec, err)
	}
	return testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
		hc.PortBindings = network.PortMap{
			p: []network.PortBinding{{HostIP: netip.IPv4Unspecified(), HostPort: fmt.Sprint(hostPort)}},
		}
	})
}
