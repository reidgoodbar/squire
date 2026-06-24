//go:build linux

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	afVSock      = 40
	sockStream   = 1
	sockCloexec  = 0o2000000
	vmAddrCIDAny = 0xffffffff
	defaultPort  = 1024
	maxPayload   = 64 * 1024 * 1024
)

type rawSockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Zero      [4]byte
}

type guestResponse struct {
	StdoutB64 string `json:"stdout_b64"`
	StderrB64 string `json:"stderr_b64"`
	ExitCode  int    `json:"exit_code"`
}

func main() {
	if os.Getenv("SQUIRE_VM_AGENT_TRANSPORT") == "serial" {
		path := os.Getenv("SQUIRE_VM_AGENT_SERIAL")
		if path == "" {
			path = "/dev/hvc0"
		}
		if err := serveSerial(path); err != nil {
			fmt.Fprintf(os.Stderr, "squire-vm-agent: %v\n", err)
			os.Exit(1)
		}
		return
	}
	port := defaultPort
	if raw := os.Getenv("SQUIRE_VM_AGENT_PORT"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || parsed == 0 {
			fmt.Fprintf(os.Stderr, "squire-vm-agent: invalid SQUIRE_VM_AGENT_PORT %q\n", raw)
			os.Exit(2)
		}
		port = int(parsed)
	}
	if err := serve(uint32(port)); err != nil {
		fmt.Fprintf(os.Stderr, "squire-vm-agent: %v\n", err)
		os.Exit(1)
	}
}

func serveSerial(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write([]byte("SQUIRE_VM_AGENT_READY\n")); err != nil {
		return err
	}
	return handleFile(file)
}

func serve(port uint32) error {
	fd, err := listenVSock(port)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	for {
		connFD, err := acceptFD(fd)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		go func() {
			_ = handleConn(connFD)
			_ = syscall.Close(connFD)
		}()
	}
}

func listenVSock(port uint32) (int, error) {
	fd, err := syscall.Socket(afVSock, sockStream|sockCloexec, 0)
	if err != nil {
		return -1, err
	}
	addr := rawSockaddrVM{
		Family: afVSock,
		Port:   port,
		CID:    vmAddrCIDAny,
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		_ = syscall.Close(fd)
		return -1, errno
	}
	if err := syscall.Listen(fd, 32); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	return fd, nil
}

func acceptFD(fd int) (int, error) {
	nfd, _, errno := syscall.RawSyscall6(syscall.SYS_ACCEPT4, uintptr(fd), 0, 0, sockCloexec, 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return int(nfd), nil
}

func handleConn(fd int) error {
	file := os.NewFile(uintptr(fd), "vsock")
	if file == nil {
		return errors.New("failed to wrap vsock fd")
	}
	defer file.Close()
	return handleFile(file)
}

func handleFile(file *os.File) error {
	var req guestRequest
	if err := json.NewDecoder(io.LimitReader(file, maxPayload)).Decode(&req); err != nil {
		return writeResponse(file, guestResponse{
			StderrB64: base64.StdEncoding.EncodeToString([]byte("invalid guest request: " + err.Error() + "\n")),
			ExitCode:  2,
		})
	}
	if req.Interactive {
		code := executeInteractive(req)
		return writeResponse(file, guestResponse{ExitCode: code})
	}
	stdout, stderr, code := execute(req)
	return writeResponse(file, guestResponse{
		StdoutB64: base64.StdEncoding.EncodeToString(stdout),
		StderrB64: base64.StdEncoding.EncodeToString(stderr),
		ExitCode:  code,
	})
}

func execute(req guestRequest) ([]byte, []byte, int) {
	if len(req.Argv) == 0 {
		return nil, []byte("missing argv\n"), 2
	}
	cwd := req.CWD
	if cwd == "" {
		cwd = "/mnt/squire-workspace"
	}
	command := req.Argv
	if squire := guestSquirePath(req.Argv[0]); squire != "" {
		command = guestSquireSessionCommand(squire, req.Argv)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = guestCommandEnv(req)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), stderr.Bytes(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode()
	}
	if stderr.Len() > 0 {
		stderr.WriteByte('\n')
	}
	stderr.WriteString(err.Error())
	stderr.WriteByte('\n')
	return stdout.Bytes(), stderr.Bytes(), 127
}

func executeInteractive(req guestRequest) int {
	if len(req.Argv) == 0 {
		return 2
	}
	cwd := req.CWD
	if cwd == "" {
		cwd = "/mnt/squire-workspace"
	}
	ttyPath := os.Getenv("SQUIRE_VM_AGENT_INTERACTIVE_SERIAL")
	if ttyPath == "" {
		ttyPath = "/dev/hvc1"
	}
	tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "squire-vm-agent: interactive tty unavailable: %v\n", err)
		return 1
	}
	defer tty.Close()
	applyTerminalSize(tty, req.TerminalRows, req.TerminalCols)
	command := req.Argv
	if squire := guestSquirePath(req.Argv[0]); squire != "" {
		command = guestSquireSessionCommand(squire, req.Argv)
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = cwd
	cmd.Env = guestCommandEnv(req)
	cmd.Stdin = tty
	cmd.Stdout = tty
	cmd.Stderr = tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	err = cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(tty, "squire-vm-agent: %v\n", err)
	return 127
}

type terminalWinsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

func applyTerminalSize(tty *os.File, rows, cols int) {
	if rows <= 0 || cols <= 0 || rows > 1000 || cols > 1000 {
		return
	}
	size := terminalWinsize{
		Row: uint16(rows),
		Col: uint16(cols),
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&size)))
}

func guestSquirePath(firstArg string) string {
	if os.Getenv("SQUIRE_VM_GUEST_NATIVE") == "1" || filepath.Base(firstArg) == "squire" {
		return ""
	}
	for _, candidate := range []string{os.Getenv("SQUIRE_VM_GUEST_SQUIRE"), "/usr/local/bin/squire", "/squire"} {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

func writeResponse(conn io.Writer, resp guestResponse) error {
	return json.NewEncoder(conn).Encode(resp)
}
