package server

import (
	"net"
	"testing"
)

// reserveLoopbackListener keeps fixture addresses distinct until each test
// releases the reservation at the corresponding Start boundary.
func reserveLoopbackListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}
