package fskitproto

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDialTimeoutIncludesUnansweredHello(t *testing.T) {
	root, err := os.MkdirTemp("", "cfd-dial-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	listener, err := net.Listen("unix", filepath.Join(root, "d.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	peer := make(chan net.Conn, 1)
	go func() {
		c, err := listener.Accept()
		if err == nil {
			peer <- c
		}
	}()
	started := time.Now()
	_, err = Dial(Descriptor{SocketPath: listener.Addr().String(), Generation: 1, Token: make([]byte, 16)}, 40*time.Millisecond)
	c := <-peer
	defer c.Close()
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("unanswered hello elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestClientCallTimeoutClosesDesynchronizedConnection(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	client := &Client{connection: a, generation: 1, maxPayload: DefaultMaxPayload, nextID: 1, timeout: 40 * time.Millisecond}
	defer client.Close()
	go func() { _, _ = ReadFrame(b, 0) }()
	started := time.Now()
	if _, err := client.Call(OpPing, nil); err == nil || time.Since(started) > time.Second {
		t.Fatalf("unanswered ping elapsed=%s err=%v", time.Since(started), err)
	}
	if _, err := client.Call(OpPing, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("timed-out transport reused: %v", err)
	}
}

func TestClientCallContextCancelsBlockedWriteAndRead(t *testing.T) {
	for _, consume := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "read"}[consume], func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()
			client := &Client{connection: a, generation: 1, maxPayload: DefaultMaxPayload, nextID: 1, timeout: time.Minute}
			defer client.Close()
			if consume {
				go func() { _, _ = ReadFrame(b, 0) }()
			}
			ctx, cancel := context.WithCancel(context.Background())
			timer := time.AfterFunc(30*time.Millisecond, cancel)
			defer timer.Stop()
			defer cancel()
			started := time.Now()
			if _, err := client.CallContext(ctx, OpPing, nil); !errors.Is(err, context.Canceled) || time.Since(started) > time.Second {
				t.Fatalf("cancel elapsed=%s err=%v", time.Since(started), err)
			}
		})
	}
}

func TestClientSuccessfulCallsDoNotInheritOldDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	client := &Client{connection: a, generation: 1, maxPayload: DefaultMaxPayload, nextID: 1, timeout: 100 * time.Millisecond}
	defer client.Close()
	go func() {
		for i := 0; i < 2; i++ {
			request, err := ReadFrame(b, 0)
			if err != nil {
				return
			}
			_ = WriteFrame(b, Frame{Kind: KindResponse, Op: request.Op, Generation: 1, RequestID: request.RequestID}, 0)
		}
	}()
	if _, err := client.Call(OpPing, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := client.Call(OpPing, nil); err != nil {
		t.Fatalf("old deadline leaked: %v", err)
	}
}
