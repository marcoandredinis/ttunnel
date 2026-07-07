package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestReadLine(t *testing.T) {
	t.Run("reads line without newline", func(t *testing.T) {
		got, err := readLine(strings.NewReader("hello\nignored"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "hello" {
			t.Fatalf("got %q, want %q", got, "hello")
		}
	})

	t.Run("returns reader error before newline", func(t *testing.T) {
		if _, err := readLine(strings.NewReader("hello")); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("rejects overlong line", func(t *testing.T) {
		_, err := readLine(strings.NewReader(strings.Repeat("x", 512) + "\n"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "message too large") {
			t.Fatalf("got error %q, want message too large", err)
		}
	})
}

func TestBadTokenRejected(t *testing.T) {
	serverAddr := startControlServer(t, "good-token")

	conn := dialTCP(t, serverAddr)
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "bad-token %d\n", freePort(t)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := readLine(conn); err == nil {
		t.Fatal("expected server to close without an acknowledgement")
	}
}

func TestAgentGetsBindFailure(t *testing.T) {
	const token = "test-token"
	serverAddr := startControlServer(t, token)
	occupied := listenTCP(t, ":0")
	defer occupied.Close()

	conn := dialTCP(t, serverAddr)
	defer conn.Close()

	port := occupied.Addr().(*net.TCPAddr).Port
	if _, err := fmt.Fprintf(conn, "%s %d\n", token, port); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := readLine(conn)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ERR failed to bind requested public port" {
		t.Fatalf("got %q, want bind failure", got)
	}
}

func TestTunnelRoundTrip(t *testing.T) {
	const token = "test-token"
	port := startEchoServer(t)
	serverAddr := startControlServer(t, token)

	ctx := t.Context()
	errc := make(chan error, 1)
	go func() {
		errc <- agentLoop(ctx, serverAddr, token, port)
	}()
	t.Cleanup(func() {
		select {
		case err := <-errc:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("agent did not stop")
		}
	})

	publicAddr := net.JoinHostPort("127.0.0.1", port)
	waitForTCP(t, publicAddr)
	if err := roundTrip(t, publicAddr, []byte("hello through tunnel")); err != nil {
		t.Fatal(err)
	}
}

func TestProxyCopiesBothDirections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		leftClient, leftProxy := net.Pipe()
		rightProxy, rightClient := net.Pipe()
		done := make(chan struct{})
		go func() {
			proxy(leftProxy, rightProxy)
			close(done)
		}()

		msg := []byte("payload")
		if _, err := leftClient.Write(msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(rightClient, buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("right side got %q, want %q", buf, msg)
		}

		reply := []byte("response")
		if _, err := rightClient.Write(reply); err != nil {
			t.Fatal(err)
		}
		buf = make([]byte, len(reply))
		if _, err := io.ReadFull(leftClient, buf); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, reply) {
			t.Fatalf("left side got %q, want %q", buf, reply)
		}

		leftClient.Close()
		rightClient.Close()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("proxy did not stop")
		}
	})
}

func startControlServer(t *testing.T, token string) string {
	t.Helper()
	ln := listenTCP(t, "127.0.0.1:0")
	ctx := t.Context()
	errc := make(chan error, 1)
	go func() {
		errc <- serveAgents(ctx, ln, token)
	}()
	t.Cleanup(func() {
		select {
		case err := <-errc:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("server did not stop")
		}
	})
	return ln.Addr().String()
}

func startEchoServer(t *testing.T) string {
	t.Helper()
	ln := listenTCP(t, "[::1]:0")
	ctx := t.Context()
	errc := make(chan error, 1)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	t.Cleanup(func() {
		select {
		case err := <-errc:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("echo server did not stop")
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					errc <- nil
				} else {
					errc <- err
				}
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)
}

func roundTrip(t *testing.T, addr string, msg []byte) error {
	t.Helper()
	conn := dialTCP(t, addr)
	defer conn.Close()

	if _, err := conn.Write(msg); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if !bytes.Equal(got, msg) {
		return fmt.Errorf("got %q, want %q", got, msg)
	}
	return nil
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		conn, err := (&net.Dialer{Timeout: 20 * time.Millisecond}).DialContext(ctx, "tcp", addr)
		if err == nil {
			conn.Close()
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", addr)
}

func dialTCP(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func freePort(t *testing.T) int {
	t.Helper()
	ln := listenTCP(t, "127.0.0.1:0")
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func listenTCP(t *testing.T, addr string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
