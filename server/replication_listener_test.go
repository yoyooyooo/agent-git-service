package server

import (
	"net"
	"testing"
)

// allocateLoopbackAddr is test-only. Production Start still binds all sockets
// atomically; fixture reservation never claims to eliminate the bind race.
func allocateLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
