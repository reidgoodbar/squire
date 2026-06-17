package kernel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const hotCacheIPCVersion = 1
const hotCacheRequestMagic uint32 = 0x31514853
const hotCacheResponseMagic uint32 = 0x31524853
const hotCacheResponseHeaderBytes = 148
const hotCacheMaxCWDBytes = 8192
const hotCacheMaxArgBytes = 8192
const hotCacheMaxArgs = 64

var hotCacheDialTimeout = 2 * time.Millisecond
var hotCacheRoundTripTimeout = 10 * time.Millisecond
var hotCacheIdleTimeout = 30 * time.Second
var hotCacheUnavailableBackoff = 100 * time.Millisecond
var hotCacheMissBackoff = 100 * time.Millisecond
var hotCacheNow = time.Now
var hotCacheDialContext = func(ctx context.Context, path string) (net.Conn, error) {
	dialer := net.Dialer{Timeout: hotCacheDialTimeout}
	return dialer.DialContext(ctx, "unix", path)
}

type hotCacheRequest struct {
	Version int
	CWD     string
	Argv    []string
}

type hotCacheResponse struct {
	Version      int
	Hit          bool
	Stdout       []byte
	Stderr       []byte
	ExitCode     int
	NativeWallMS int64
	Family       OperatorFamily
	StdoutHash   string
	StderrHash   string
	Reason       string
}

type hotCacheServer struct {
	socketPath string
	listener   net.Listener
}

type hotCacheClient struct {
	path string
	conn net.Conn
}

func startHotCacheServer(ctx context.Context, k *Kernel, storeRoot string) (*hotCacheServer, error) {
	path := hotCacheSocketPath(storeRoot)
	if path == "" {
		return nil, errors.New("hot cache socket unavailable on this platform")
	}
	_ = os.Remove(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	server := &hotCacheServer{socketPath: path, listener: listener}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	go server.serve(ctx, k)
	return server, nil
}

func (s *hotCacheServer) Close() error {
	if s == nil {
		return nil
	}
	if s.listener != nil {
		_ = s.listener.Close()
	}
	if s.socketPath != "" {
		_ = os.Remove(s.socketPath)
	}
	return nil
}

func (s *hotCacheServer) serve(ctx context.Context, k *Kernel) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go handleHotCacheConn(ctx, k, conn)
	}
}

func handleHotCacheConn(ctx context.Context, k *Kernel, conn net.Conn) {
	defer conn.Close()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(hotCacheIdleTimeout))
		req, err := readHotCacheRequest(conn)
		if err != nil {
			_ = conn.SetWriteDeadline(time.Now().Add(hotCacheRoundTripTimeout))
			_, _ = conn.Write(encodeHotCacheMissFrame())
			return
		}
		frame := encodeHotCacheMissFrame()
		if req.Version == hotCacheIPCVersion && len(req.Argv) > 0 {
			inv := NormalizeInvocation(req.CWD, req.Argv)
			var phases PhaseTimings
			if candidate, _, ok := k.findPreparedReplay(inv, &phases); ok {
				frame = candidate.HotCacheFrame
				if len(frame) == 0 {
					frame = encodeHotCacheHitFrame(candidate.Stdout, candidate.Stderr, candidate.Observation.ExitCode)
				}
			}
		}
		_ = conn.SetWriteDeadline(time.Now().Add(hotCacheRoundTripTimeout))
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

func (k *Kernel) tryDaemonReplay(ctx context.Context, inv CommandInvocation, family OperatorFamily, phases *PhaseTimings) (RunResult, bool) {
	if k.Store == nil || !isHotPreparedReplayCandidate(inv.PolicyArgv) {
		return RunResult{}, false
	}
	if replay, ok := k.tryHotSnapshotReplay(inv, family, phases); ok {
		return replay, true
	}
	path := hotCacheSocketPath(k.Store.Root)
	if path == "" {
		return RunResult{}, false
	}
	missKey := hotCacheMissKey(inv)
	if k.shouldSkipHotCache(path, missKey) {
		return RunResult{}, false
	}
	req := hotCacheRequest{Version: hotCacheIPCVersion, CWD: inv.OriginalCWD, Argv: inv.OriginalArgv}
	resp, err := k.hotCacheRoundTrip(ctx, path, req)
	if err != nil {
		k.recordHotCacheUnavailable(path)
		return RunResult{}, false
	}
	if resp.Version != hotCacheIPCVersion || !resp.Hit {
		k.recordHotCacheMiss(missKey)
		return RunResult{}, false
	}
	if hashBytes(resp.Stdout) != resp.StdoutHash || hashBytes(resp.Stderr) != resp.StderrHash {
		k.recordHotCacheMiss(missKey)
		return RunResult{}, false
	}
	k.recordHotCacheHit(path, missKey)
	return RunResult{
		Stdout:   append([]byte(nil), resp.Stdout...),
		Stderr:   append([]byte(nil), resp.Stderr...),
		ExitCode: resp.ExitCode,
		Mode:     ModeReplay,
		Family:   family,
		Proof: &ProofRecord{
			OperationKeyMatched:        true,
			InputFingerprintsMatched:   true,
			InvalidationEpochUnchanged: true,
			OperatorAllowlisted:        IsReplayAllowed(inv.PolicyArgv),
			OutputAvailable:            true,
			OutputExact:                true,
			PolicyAllowedReplay:        true,
			NativeFallbackAvailable:    true,
			OperationKey:               "ipc-hot-cache",
		},
		Phases: *phases,
	}, true
}

func (k *Kernel) hotCacheRoundTrip(ctx context.Context, path string, req hotCacheRequest) (hotCacheResponse, error) {
	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	hadConn := k.hotCacheClient != nil && k.hotCacheClient.conn != nil && k.hotCacheClient.path == path
	resp, err := k.hotCacheRoundTripLocked(ctx, path, req)
	if err == nil {
		return resp, nil
	}
	k.closeHotCacheClientLocked()
	if !hadConn {
		return hotCacheResponse{}, err
	}
	return k.hotCacheRoundTripLocked(ctx, path, req)
}

func (k *Kernel) hotCacheRoundTripLocked(ctx context.Context, path string, req hotCacheRequest) (hotCacheResponse, error) {
	if k.hotCacheClient == nil || k.hotCacheClient.conn == nil || k.hotCacheClient.path != path {
		k.closeHotCacheClientLocked()
		conn, err := hotCacheDialContext(ctx, path)
		if err != nil {
			return hotCacheResponse{}, err
		}
		k.hotCacheClient = &hotCacheClient{path: path, conn: conn}
	}
	conn := k.hotCacheClient.conn
	_ = conn.SetDeadline(time.Now().Add(hotCacheRoundTripTimeout))
	if err := writeHotCacheRequest(conn, req); err != nil {
		return hotCacheResponse{}, err
	}
	return readHotCacheResponse(conn)
}

func (k *Kernel) closeHotCacheClientLocked() {
	if k.hotCacheClient == nil {
		return
	}
	if k.hotCacheClient.conn != nil {
		_ = k.hotCacheClient.conn.Close()
	}
	k.hotCacheClient = nil
}

func (k *Kernel) shouldSkipHotCache(path, missKey string) bool {
	now := hotCacheNow()
	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	if k.hotCacheUnavailablePath == path && now.Before(k.hotCacheUnavailableUntil) {
		return true
	}
	if k.hotCacheMisses != nil {
		if until, ok := k.hotCacheMisses[missKey]; ok {
			if now.Before(until) {
				return true
			}
			delete(k.hotCacheMisses, missKey)
		}
	}
	return false
}

func (k *Kernel) recordHotCacheUnavailable(path string) {
	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	k.hotCacheUnavailablePath = path
	k.hotCacheUnavailableUntil = hotCacheNow().Add(hotCacheUnavailableBackoff)
}

func (k *Kernel) recordHotCacheMiss(missKey string) {
	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	if k.hotCacheMisses == nil {
		k.hotCacheMisses = map[string]time.Time{}
	}
	k.hotCacheMisses[missKey] = hotCacheNow().Add(hotCacheMissBackoff)
}

func (k *Kernel) recordHotCacheHit(path, missKey string) {
	k.hotCacheMu.Lock()
	defer k.hotCacheMu.Unlock()
	if k.hotCacheUnavailablePath == path {
		k.hotCacheUnavailableUntil = time.Time{}
	}
	if k.hotCacheMisses != nil {
		delete(k.hotCacheMisses, missKey)
	}
}

func hotCacheMissKey(inv CommandInvocation) string {
	return hashString(absPath(inv.PolicyCWD) + "|" + normalizeArgv(inv.PolicyArgv))
}

func hotCacheSocketPath(storeRoot string) string {
	if runtime.GOOS == "windows" || storeRoot == "" {
		return ""
	}
	dir := hotCacheSocketDir()
	if dir == "" {
		return ""
	}
	name := "sqk-" + hashString(storeRoot)[:20] + ".sock"
	path := filepath.Join(dir, name)
	if len(path) > 100 {
		path = filepath.Join("/private/tmp", name)
	}
	if len(path) > 100 {
		path = filepath.Join("/tmp", name)
	}
	return path
}

func hotCacheSocketDir() string {
	if dir := os.Getenv("SQUIRE_KERNEL_SOCKET_DIR"); dir != "" {
		return filepath.Clean(dir)
	}
	dir := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != "" {
		dir = resolved
	} else if runtime.GOOS == "darwin" && strings.HasPrefix(dir, "/var/") {
		dir = "/private" + dir
	}
	return filepath.Clean(dir)
}

func writeHotCacheRequest(w io.Writer, req hotCacheRequest) error {
	if len(req.CWD) > hotCacheMaxCWDBytes || len(req.Argv) == 0 || len(req.Argv) > hotCacheMaxArgs {
		return errors.New("hot cache request too large")
	}
	var buf bytes.Buffer
	var header [12]byte
	binary.LittleEndian.PutUint32(header[0:4], hotCacheRequestMagic)
	binary.LittleEndian.PutUint16(header[4:6], hotCacheIPCVersion)
	binary.LittleEndian.PutUint16(header[6:8], uint16(len(req.Argv)))
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(req.CWD)))
	buf.Write(header[:])
	buf.WriteString(req.CWD)
	for _, arg := range req.Argv {
		if len(arg) > hotCacheMaxArgBytes {
			return errors.New("hot cache argument too large")
		}
		var lenBuf [4]byte
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(arg)))
		buf.Write(lenBuf[:])
		buf.WriteString(arg)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

func readHotCacheRequest(r io.Reader) (hotCacheRequest, error) {
	var header [12]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return hotCacheRequest{}, err
	}
	if binary.LittleEndian.Uint32(header[0:4]) != hotCacheRequestMagic {
		return hotCacheRequest{}, errors.New("invalid hot cache request magic")
	}
	version := int(binary.LittleEndian.Uint16(header[4:6]))
	argc := int(binary.LittleEndian.Uint16(header[6:8]))
	cwdLen := int(binary.LittleEndian.Uint32(header[8:12]))
	if argc <= 0 || argc > hotCacheMaxArgs || cwdLen < 0 || cwdLen > hotCacheMaxCWDBytes {
		return hotCacheRequest{}, errors.New("invalid hot cache request size")
	}
	cwdBytes := make([]byte, cwdLen)
	if _, err := io.ReadFull(r, cwdBytes); err != nil {
		return hotCacheRequest{}, err
	}
	argv := make([]string, 0, argc)
	for i := 0; i < argc; i++ {
		var lenBuf [4]byte
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return hotCacheRequest{}, err
		}
		argLen := int(binary.LittleEndian.Uint32(lenBuf[:]))
		if argLen < 0 || argLen > hotCacheMaxArgBytes {
			return hotCacheRequest{}, errors.New("invalid hot cache argument size")
		}
		argBytes := make([]byte, argLen)
		if _, err := io.ReadFull(r, argBytes); err != nil {
			return hotCacheRequest{}, err
		}
		argv = append(argv, string(argBytes))
	}
	return hotCacheRequest{Version: version, CWD: string(cwdBytes), Argv: argv}, nil
}

func encodeHotCacheMissFrame() []byte {
	var frame [hotCacheResponseHeaderBytes]byte
	binary.LittleEndian.PutUint32(frame[0:4], hotCacheResponseMagic)
	binary.LittleEndian.PutUint16(frame[4:6], hotCacheIPCVersion)
	return frame[:]
}

func encodeHotCacheHitFrame(stdout, stderr []byte, exitCode int) []byte {
	if len(stdout)+len(stderr) > maxFastPathOutputBytes {
		return encodeHotCacheMissFrame()
	}
	outHash := hashBytes(stdout)
	errHash := hashBytes(stderr)
	frame := make([]byte, hotCacheResponseHeaderBytes+len(stdout)+len(stderr))
	binary.LittleEndian.PutUint32(frame[0:4], hotCacheResponseMagic)
	binary.LittleEndian.PutUint16(frame[4:6], hotCacheIPCVersion)
	binary.LittleEndian.PutUint16(frame[6:8], 1)
	binary.LittleEndian.PutUint32(frame[8:12], uint32(int32(exitCode)))
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(stdout)))
	binary.LittleEndian.PutUint32(frame[16:20], uint32(len(stderr)))
	copy(frame[20:84], outHash)
	copy(frame[84:148], errHash)
	copy(frame[148:], stdout)
	copy(frame[148+len(stdout):], stderr)
	return frame
}

func readHotCacheResponse(r io.Reader) (hotCacheResponse, error) {
	header := make([]byte, hotCacheResponseHeaderBytes)
	if _, err := io.ReadFull(r, header); err != nil {
		return hotCacheResponse{}, err
	}
	if binary.LittleEndian.Uint32(header[0:4]) != hotCacheResponseMagic {
		return hotCacheResponse{}, errors.New("invalid hot cache response magic")
	}
	version := int(binary.LittleEndian.Uint16(header[4:6]))
	status := binary.LittleEndian.Uint16(header[6:8])
	if status == 0 {
		return hotCacheResponse{Version: version, Reason: "miss"}, nil
	}
	exitCode := int(int32(binary.LittleEndian.Uint32(header[8:12])))
	stdoutLen := int(binary.LittleEndian.Uint32(header[12:16]))
	stderrLen := int(binary.LittleEndian.Uint32(header[16:20]))
	if stdoutLen < 0 || stderrLen < 0 || stdoutLen+stderrLen > maxFastPathOutputBytes {
		return hotCacheResponse{}, errors.New("invalid hot cache response size")
	}
	stdoutHash := string(header[20:84])
	stderrHash := string(header[84:148])
	if _, err := hex.DecodeString(stdoutHash); err != nil {
		return hotCacheResponse{}, errors.New("invalid stdout hash")
	}
	if _, err := hex.DecodeString(stderrHash); err != nil {
		return hotCacheResponse{}, errors.New("invalid stderr hash")
	}
	payload := make([]byte, stdoutLen+stderrLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return hotCacheResponse{}, err
	}
	return hotCacheResponse{
		Version:    version,
		Hit:        true,
		Stdout:     append([]byte(nil), payload[:stdoutLen]...),
		Stderr:     append([]byte(nil), payload[stdoutLen:]...),
		ExitCode:   exitCode,
		StdoutHash: stdoutHash,
		StderrHash: stderrHash,
	}, nil
}
