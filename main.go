package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/hashicorp/yamux"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "server" {
		runServer()
	} else if len(os.Args) > 1 && os.Args[1] == "agent" {
		runAgent(os.Args[2:])
	} else {
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `ttunnel - reverse TCP tunnel
Usage:
  ttunnel server
  TTUNNEL_TOKEN=<token> ttunnel agent --server <server-ip>:8001 [--target <exposed-port>]`)
}

func runServer() {
	token, err := generateToken()
	if err != nil {
		fatal(err)
	}
	ln, err := net.Listen("tcp", ":8001")
	if err != nil {
		fatal(fmt.Errorf("listen: %w", err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serveAgents(ctx, ln, token); err != nil {
		fatal(err)
	}
}

func runAgent(args []string) {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	serverAddr := fs.String("server", "", "server address, e.g. host:8001")
	target := fs.String("target", "443", "local and exposed port, e.g. 443")
	fs.Parse(args)

	token := os.Getenv("TTUNNEL_TOKEN")
	if *serverAddr == "" || token == "" || *target == "" {
		fatal(fmt.Errorf("agent: TTUNNEL_TOKEN and --server are required"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := agentLoop(ctx, *serverAddr, token, *target); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func serveAgents(ctx context.Context, ln net.Listener, token string) error {
	fmt.Fprintf(os.Stdout, "TTUNNEL_TOKEN=%s\n", token)
	slog.Info("server listening", "addr", ln.Addr().String())
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			slog.Error("accept failed", "error", err)
			continue
		}
		go handleAgent(ctx, slog.With("agent_addr", conn.RemoteAddr().String()), conn, token)
	}
}

func handleAgent(ctx context.Context, log *slog.Logger, conn net.Conn, token string) {
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	line, err := readLine(conn)
	if err != nil {
		log.Error("handshake failed", "error", err)
		return
	}
	gotToken, port, ok := strings.Cut(line, " ")
	if !ok {
		log.Error("bad handshake")
		return
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(gotToken)) != 1 {
		log.Warn("unauthorized agent")
		return
	}

	public, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		_, _ = io.WriteString(conn, "ERR failed to bind requested public port\n")
		log.Error("public listen failed", "port", port, "error", err)
		return
	}
	defer public.Close()
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		log.Error("response failed", "error", err)
		return
	}

	session, err := yamux.Server(conn, nil)
	if err != nil {
		log.Error("yamux server failed", "error", err)
		return
	}
	defer session.Close()
	go func() {
		select {
		case <-ctx.Done():
			session.Close()
		case <-session.CloseChan():
		}
		public.Close()
	}()
	log.Info("agent connected", "listen", port)

	for {
		client, err := public.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Error("public accept failed", "error", err)
			return
		}
		go func() {
			stream, err := session.Open()
			if err != nil {
				log.Error("open stream failed", "error", err)
				client.Close()
				return
			}
			proxy(client, stream)
		}()
	}
}

func agentLoop(ctx context.Context, serverAddr, token string, port string) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, fmt.Sprintf("%s %s\n", token, port)); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("server rejected tunnel: %s", line)
	}
	session, err := yamux.Client(conn, nil)
	if err != nil {
		return fmt.Errorf("yamux client: %w", err)
	}
	defer session.Close()
	go func() {
		<-ctx.Done()
		session.Close()
	}()
	slog.Info("tunnel established", "port", port)

	for {
		stream, err := session.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("session closed: %w", err)
		}
		go func() {
			local, err := net.Dial("tcp", "localhost:"+port)
			if err != nil {
				slog.Error("local dial failed", "target", "localhost:"+port, "error", err)
				stream.Close()
				return
			}
			proxy(stream, local)
		}()
	}
}

func readLine(r io.Reader) (string, error) {
	const maxMessageLen = 512
	var b []byte
	var one [1]byte
	for len(b) < maxMessageLen {
		n, err := r.Read(one[:])
		if n == 1 {
			if one[0] == '\n' {
				return string(b), nil
			}
			b = append(b, one[0])
		}
		if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("message too large: %d bytes", maxMessageLen)
}

func proxy(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	cp := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface {
			CloseWrite() error
		}); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Go(func() { cp(a, b) })
	wg.Go(func() { cp(b, a) })
	wg.Wait()
	a.Close()
	b.Close()
}
