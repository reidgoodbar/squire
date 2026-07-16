package proofcache

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	prepareRequestDirName  = "prepare_requests"
	prepareRequestMaxBytes = 64 * 1024
	prepareRequestMaxArgc  = 64
	prepareRequestMaxArg   = 4096
)

var prepareRequestMagic = [8]byte{'S', 'Q', 'R', 'Q', '0', '0', '0', '1'}

type prepareRequest struct {
	CWD  string
	Argv []string
}

type prepareRequestCycle struct {
	Observed int
	Prepared int
	Rejected int
}

func decodePrepareRequest(data []byte) (prepareRequest, error) {
	if len(data) < 16 || len(data) > prepareRequestMaxBytes || string(data[:8]) != string(prepareRequestMagic[:]) {
		return prepareRequest{}, errors.New("invalid prepare request header")
	}
	cwdLen := int(binary.LittleEndian.Uint32(data[8:12]))
	argc := int(binary.LittleEndian.Uint32(data[12:16]))
	if cwdLen <= 0 || cwdLen >= prepareRequestMaxArg || argc <= 0 || argc > prepareRequestMaxArgc || 16+cwdLen > len(data) {
		return prepareRequest{}, errors.New("invalid prepare request dimensions")
	}
	cwd := string(data[16 : 16+cwdLen])
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, '\x00') || filepath.Clean(cwd) != cwd {
		return prepareRequest{}, errors.New("invalid prepare request cwd")
	}
	offset := 16 + cwdLen
	argv := make([]string, 0, argc)
	for i := 0; i < argc; i++ {
		if offset+4 > len(data) {
			return prepareRequest{}, errors.New("truncated prepare request argument")
		}
		argLen := int(binary.LittleEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if argLen <= 0 || argLen >= prepareRequestMaxArg || offset+argLen > len(data) {
			return prepareRequest{}, errors.New("invalid prepare request argument")
		}
		arg := string(data[offset : offset+argLen])
		offset += argLen
		if strings.ContainsRune(arg, '\x00') {
			return prepareRequest{}, errors.New("prepare request argument contains NUL")
		}
		argv = append(argv, arg)
	}
	if offset != len(data) {
		return prepareRequest{}, errors.New("prepare request has trailing bytes")
	}
	return prepareRequest{CWD: cwd, Argv: argv}, nil
}

func validPrepareRequest(k *Engine, filename string, req prepareRequest) bool {
	if k == nil || k.Store == nil || !strings.HasSuffix(filename, ".req") {
		return false
	}
	key := strings.TrimSuffix(filename, ".req")
	if !validHotSnapshotHash(key) || preparedReplayLookupKey(req.CWD, req.Argv) != key {
		return false
	}
	inv := NormalizeInvocation(req.CWD, req.Argv)
	if inv.PolicyCWD != absPath(req.CWD) || normalizeArgv(inv.PolicyArgv) != normalizeArgv(req.Argv) ||
		!IsProofGatedReplayCandidate(inv.PolicyArgv) || !isHotPreparedReplayCandidate(inv.PolicyArgv) {
		return false
	}
	repoRoot, requestStore, ok := FastWorkspace(req.CWD)
	return ok && pathWithinRoot(req.CWD, repoRoot) && absPath(requestStore) == absPath(k.Store.Root)
}

func (k *Engine) consumePrepareRequests(ctx context.Context, limit int) (prepareRequestCycle, error) {
	var cycle prepareRequestCycle
	if k == nil || k.Store == nil {
		return cycle, nil
	}
	if limit <= 0 {
		limit = 32
	}
	dir := filepath.Join(k.Store.Root, prepareRequestDirName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return cycle, nil
	}
	if err != nil {
		return cycle, err
	}
	if k.Oracle == nil {
		k.Oracle = NewRepoOracle()
	}
	var ledger *ValidityLedger
	var phases PhaseTimings
	var preparedPaths []string
	for _, entry := range entries {
		if cycle.Observed >= limit || !strings.HasSuffix(entry.Name(), ".req") {
			continue
		}
		cycle.Observed++
		path := filepath.Join(dir, entry.Name())
		info, infoErr := os.Lstat(path)
		if infoErr != nil {
			continue
		}
		if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > prepareRequestMaxBytes {
			cycle.Rejected++
			_ = os.Remove(path)
			continue
		}
		data, readErr := os.ReadFile(path)
		req, decodeErr := decodePrepareRequest(data)
		if readErr != nil || decodeErr != nil || !validPrepareRequest(k, entry.Name(), req) {
			cycle.Rejected++
			_ = os.Remove(path)
			continue
		}
		inv := NormalizeInvocation(req.CWD, req.Argv)
		if _, ok := readHotSnapshotResponse(hotCacheSnapshotPath(k.Store.Root), inv); ok {
			cycle.Prepared++
			_ = os.Remove(path)
			continue
		}
		if ledger == nil {
			if err := k.Store.Init(); err != nil {
				return cycle, err
			}
			ledger, err = k.Store.Load()
			if err != nil {
				return cycle, err
			}
		}
		ws := k.Oracle.ShadowSnapshot(ctx, req.CWD)
		if k.prewarmProofGatedCandidate(ctx, req.CWD, "maintainer-demand", req.Argv, ws, ledger, &phases,
			"foreground-safe miss requested exact background preparation; native fallback remains available") {
			cycle.Prepared++
			preparedPaths = append(preparedPaths, path)
			continue
		}
		cycle.Rejected++
		_ = os.Remove(path)
	}
	if len(preparedPaths) == 0 {
		return cycle, nil
	}
	if err := k.finishWarm(ledger, &phases); err != nil {
		return cycle, fmt.Errorf("publish demanded hot snapshot: %w", err)
	}
	for _, path := range preparedPaths {
		_ = os.Remove(path)
	}
	return cycle, nil
}
