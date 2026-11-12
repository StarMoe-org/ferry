package kube

import (
	"context"
	"net"
	"testing"
	"time"
)

// A port that takes the connection and then says nothing (a cancelled node behind a load balancer
// or NAT) must fail at the TLS handshake limit, not hold the request for the whole client timeout.
func TestSilentServerFailsAtHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold it open, never answer
		}
	}()

	oldTLS, oldReq := TLSHandshakeTimeout, RequestTimeout
	TLSHandshakeTimeout, RequestTimeout = 200*time.Millisecond, 10*time.Second
	defer func() { TLSHandshakeTimeout, RequestTimeout = oldTLS, oldReq }()

	cl, err := New(Config{Name: "gone", Server: "https://" + ln.Addr().String(), Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := cl.Version(context.Background()); err == nil {
		t.Fatal("expected an error from a silent server")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("silent server held the request for %s", took)
	}
}
