package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"squire.run/internal/kernel"
)

var shimHelperMagic = [8]byte{'S', 'Q', 'S', 'H', 'I', 'M', '1', 0}

const shimHelperMaxRequestBytes = 1 << 20

func runKernelShimHelper(ctx context.Context, defaultCWD string, args []string) error {
	socketPath, err := parseShimHelperSocket(args)
	if err != nil {
		return err
	}
	if realPath := os.Getenv("SQUIRE_SHIM_REAL_PATH"); realPath != "" {
		_ = os.Setenv("PATH", realPath)
	}
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = ln.Close()
		_ = os.Remove(socketPath)
	}()
	_ = os.Chmod(socketPath, 0o600)

	sessionID := os.Getenv("SQUIRE_KERNEL_SESSION_ID")
	if sessionID == "" {
		sessionID = "shim-helper"
	}
	server := &adapterServer{
		defaultCWD:       defaultCWD,
		defaultSessionID: sessionID,
		ensureMaintainer: true,
		kernels:          make(map[string]*kernel.Kernel),
		states:           make(map[string]adapterCWDState),
		plans:            make(map[string]adapterCommandPlan),
		hotMisses:        make(map[string]time.Time),
		maintainers:      make(map[string]adapterMaintainerMemo),
	}
	server.primeDefaultRepo(ctx)

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return err
			}
		}
		handleShimHelperConn(ctx, server, conn)
	}
}

func parseShimHelperSocket(args []string) (string, error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--socket" {
			return "", fmt.Errorf("unknown kernel shim-helper option %q", args[i])
		}
		if i+1 >= len(args) || args[i+1] == "" {
			return "", fmt.Errorf("kernel shim-helper --socket requires a path")
		}
		return args[i+1], nil
	}
	return "", fmt.Errorf("kernel shim-helper requires --socket <path>")
}

func handleShimHelperConn(ctx context.Context, server *adapterServer, conn net.Conn) {
	defer conn.Close()
	data, err := io.ReadAll(io.LimitReader(conn, shimHelperMaxRequestBytes))
	if err != nil {
		_ = writeShimHelperError(conn, "read request failed: "+err.Error())
		return
	}
	cwd, argv, err := parseShimHelperRequest(data)
	if err != nil {
		_ = writeShimHelperError(conn, err.Error())
		return
	}
	resp := server.handleRequest(ctx, adapterRequest{
		CWD:       cwd,
		Argv:      argv,
		SessionID: server.defaultSessionID,
	})
	_ = writeShimHelperResponse(conn, resp)
}

func parseShimHelperRequest(data []byte) (string, []string, error) {
	data = bytes.TrimRight(data, "\x00")
	parts := bytes.Split(data, []byte{0})
	if len(parts) < 2 || len(parts[0]) == 0 {
		return "", nil, fmt.Errorf("invalid shim helper request")
	}
	argv := make([]string, 0, len(parts)-1)
	for _, part := range parts[1:] {
		if len(part) == 0 {
			continue
		}
		argv = append(argv, string(part))
	}
	if len(argv) == 0 {
		return "", nil, fmt.Errorf("shim helper request missing argv")
	}
	return string(parts[0]), argv, nil
}

func writeShimHelperError(w io.Writer, msg string) error {
	return writeShimHelperFrame(w, 127, 0, nil, []byte(msg+"\n"))
}

func writeShimHelperResponse(w io.Writer, resp adapterResponse) error {
	if !resp.OK {
		return writeShimHelperError(w, resp.Error)
	}
	stdout, err := base64.StdEncoding.DecodeString(resp.StdoutB64)
	if err != nil {
		return writeShimHelperError(w, "invalid adapter stdout: "+err.Error())
	}
	stderr, err := base64.StdEncoding.DecodeString(resp.StderrB64)
	if err != nil {
		return writeShimHelperError(w, "invalid adapter stderr: "+err.Error())
	}
	return writeShimHelperFrame(w, resp.ExitCode, shimHelperMode(resp.Mode), stdout, stderr)
}

func shimHelperMode(mode kernel.Mode) uint32 {
	switch mode {
	case kernel.ModeReplay:
		return 1
	case kernel.ModeNever:
		return 3
	case kernel.ModeNative:
		return 2
	default:
		return 0
	}
}

func writeShimHelperFrame(w io.Writer, exitCode int, mode uint32, stdout, stderr []byte) error {
	header := make([]byte, 24)
	copy(header[0:8], shimHelperMagic[:])
	binary.LittleEndian.PutUint32(header[8:12], uint32(int32(exitCode)))
	binary.LittleEndian.PutUint32(header[12:16], mode)
	binary.LittleEndian.PutUint32(header[16:20], uint32(len(stdout)))
	binary.LittleEndian.PutUint32(header[20:24], uint32(len(stderr)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(stdout) > 0 {
		if _, err := w.Write(stdout); err != nil {
			return err
		}
	}
	if len(stderr) > 0 {
		if _, err := w.Write(stderr); err != nil {
			return err
		}
	}
	return nil
}
