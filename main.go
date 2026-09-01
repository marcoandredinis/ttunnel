package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

//go:embed README.md
var readme string

const handshakeTimeout = 10 * time.Second

func showUsage() {
	fmt.Print(readme)
	os.Exit(2)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "server":
		err = runServer(ctx)
	case len(os.Args) > 2 && os.Args[1] == "agent":
		err = runAgent(ctx, os.Args[2:])
	default:
		showUsage()
	}
	if err != nil {
		log.Fatalf("error: %v", err)
	}
}

func runServer(ctx context.Context) error {
	ln, err := net.Listen("tcp", ":8001")
	if err != nil {
		return err
	}
	return serveAgents(ctx, ln, rand.Text())
}

func runAgent(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	fs.Usage = showUsage
	target := fs.String("target", "443", "local and exposed port")
	fs.Parse(args[1:])
	token := os.Getenv("TTUNNEL_TOKEN")
	if fs.NArg() != 0 || *target == "" || token == "" {
		showUsage()
	}
	return agentLoop(ctx, net.JoinHostPort(args[0], "8001"), token, *target)
}

func serveAgents(ctx context.Context, ln net.Listener, token string) error {
	fmt.Printf("TTUNNEL_TOKEN=%s\n", token)
	slog.Info("server listening", "addr", ln.Addr().String())
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			slog.ErrorContext(ctx, "accept failed", "error", err)
			time.Sleep(time.Second)
			continue
		}
		go func() {
			if err := handleAgent(ctx, slog.With("agent_addr", conn.RemoteAddr().String()), conn, token); err != nil {
				slog.WarnContext(ctx, "agent handler failed", "error", err)
			}
		}()
	}
}

func handleAgent(ctx context.Context, log *slog.Logger, conn net.Conn, token string) error {
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}
	gotToken, port, ok := strings.Cut(line, " ")
	if !ok {
		return errors.New("bad handshake")
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(gotToken)) != 1 {
		return errors.New("unauthorized agent")
	}

	public, err := net.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		_, _ = io.WriteString(conn, "ERR failed to bind requested public port\n")
		return fmt.Errorf("public listen failed: %w", err)
	}
	defer public.Close()
	if _, err := io.WriteString(conn, "OK\n"); err != nil {
		return fmt.Errorf("response failed: %w", err)
	}

	_ = conn.SetDeadline(time.Time{})
	session, err := yamux.Server(conn, nil)
	if err != nil {
		return fmt.Errorf("yamux server failed: %w", err)
	}
	defer session.Close()
	defer context.AfterFunc(ctx, func() { _ = session.Close() })()
	go func() { <-session.CloseChan(); _ = public.Close() }()
	log.Info("agent connected", "listen", port)

	for {
		client, err := public.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("public accept failed: %w", err)
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

func agentLoop(ctx context.Context, serverAddr, token, port string) error {
	conn, err := (&net.Dialer{Timeout: handshakeTimeout}).DialContext(ctx, "tcp", serverAddr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	if _, err := fmt.Fprintf(conn, "%s %s\n", token, port); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	line, err := readLine(conn)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if line != "OK" {
		return fmt.Errorf("server rejected tunnel: %s", line)
	}
	_ = conn.SetDeadline(time.Time{})
	session, err := yamux.Client(conn, nil)
	if err != nil {
		return fmt.Errorf("yamux client: %w", err)
	}
	defer session.Close()
	defer context.AfterFunc(ctx, func() { _ = session.Close() })()
	slog.InfoContext(ctx, "tunnel established", "port", port)

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
				slog.ErrorContext(ctx, "local dial failed", "target", "localhost:"+port, "error", err)
				stream.Close()
				return
			}
			proxy(stream, local)
		}()
	}
}

func readLine(r io.Reader) (string, error) {
	const maxMessageLen = 128
	b, one := make([]byte, 0, maxMessageLen), [1]byte{}
	for len(b) < maxMessageLen {
		if _, err := io.ReadFull(r, one[:]); err != nil {
			return "", err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return "", fmt.Errorf("message too large: %d bytes", maxMessageLen)
}

func proxy(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	cp := func(dst, src io.ReadWriteCloser) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
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
