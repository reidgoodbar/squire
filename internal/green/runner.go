package green

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os/exec"
	"time"
)

type digestWriter struct {
	hash  hash.Hash
	bytes int64
}

func newDigestWriter() *digestWriter {
	return &digestWriter{hash: sha256.New()}
}

func (writer *digestWriter) Write(value []byte) (int, error) {
	written, err := writer.hash.Write(value)
	writer.bytes += int64(written)
	return written, err
}

func (writer *digestWriter) digest() string {
	return hex.EncodeToString(writer.hash.Sum(nil))
}

func runCheck(ctx context.Context, repoRoot string, config Config, check Check, store *stateStore) CheckRecord {
	started := time.Now()
	record := CheckRecord{Name: check.Name, State: "error", StartedAt: started, ExitCode: -1}
	guard, err := startMutationGuard(repoRoot, check)
	if err != nil {
		record.Error = "input mutation guard unavailable: " + err.Error()
		record.CompletedAt = time.Now()
		record.Duration = time.Since(started)
		_ = publishRecord(store, repoRoot, config.Digest, record)
		return record
	}
	defer guard.Close()

	workspace := observeWorkspace(ctx, repoRoot)
	proof, err := computeCheckProofAtWorkspace(ctx, repoRoot, config, check, workspace.ID)
	if err != nil {
		record.Error = err.Error()
		record.CompletedAt = time.Now()
		record.Duration = time.Since(started)
		_ = publishRecord(store, repoRoot, config.Digest, record)
		return record
	}
	populateRecordProof(&record, proof)
	trusted, trustErr := ConfigTrusted(repoRoot, store.root, config.Digest)
	if trustErr != nil || !trusted {
		record.State = "discarded"
		record.Error = "Green config trust changed before validation started"
		if trustErr != nil {
			record.Error += ": " + trustErr.Error()
		}
		record.CompletedAt = time.Now()
		record.Duration = time.Since(started)
		_ = publishRecord(store, repoRoot, config.Digest, record)
		return record
	}
	if changed, reason := guard.Changed(); changed {
		record.State = "discarded"
		record.Error = "inputs changed during preflight: " + reason
		record.CompletedAt = time.Now()
		record.Duration = time.Since(started)
		_ = publishRecord(store, repoRoot, config.Digest, record)
		return record
	}

	previous, _ := store.load(repoRoot)
	if prior, ok := previous.Checks[check.Name]; ok {
		record.Attempt = prior.Attempt + 1
	} else {
		record.Attempt = 1
	}
	record.State = "running"
	_ = publishRecord(store, repoRoot, config.Digest, record)

	checkCtx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()
	command := exec.CommandContext(checkCtx, proof.ExecutablePath, check.Command[1:]...)
	command.Args[0] = check.Command[0]
	command.Dir = proof.CWD
	command.Env = proof.Environment
	stdout := newDigestWriter()
	stderr := newDigestWriter()
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		record.State = "error"
		record.Error = "start native validation: " + err.Error()
		record.CompletedAt = time.Now()
		record.Duration = time.Since(started)
		_ = publishRecord(store, repoRoot, config.Digest, record)
		return record
	}
	record.PID = command.Process.Pid
	lowerProcessPriority(record.PID)
	_ = publishRecord(store, repoRoot, config.Digest, record)

	waitErr := command.Wait()
	record.PID = 0
	record.StdoutDigest = stdout.digest()
	record.StderrDigest = stderr.digest()
	record.StdoutBytes = stdout.bytes
	record.StderrBytes = stderr.bytes
	record.ExitCode = 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			record.ExitCode = exitErr.ExitCode()
		} else {
			record.ExitCode = -1
			record.Error = waitErr.Error()
		}
	}
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		record.State = "timed_out"
		record.Error = "validation exceeded " + check.Timeout.String()
	} else if record.ExitCode == 0 {
		record.State = "passed"
	} else if record.ExitCode >= 0 {
		record.State = "failed"
	} else {
		record.State = "error"
	}

	postWorkspace := observeWorkspace(ctx, repoRoot)
	postProof, postErr := computeCheckProofAtWorkspace(ctx, repoRoot, config, check, postWorkspace.ID)
	trusted, trustErr = ConfigTrusted(repoRoot, store.root, config.Digest)
	if trustErr != nil || !trusted {
		record.State = "discarded"
		record.Error = "Green config trust changed while validation ran"
		if trustErr != nil {
			record.Error += ": " + trustErr.Error()
		}
	} else if changed, reason := guard.Changed(); changed {
		record.State = "discarded"
		record.Error = "declared inputs changed while validation ran: " + reason
	} else if postErr != nil {
		record.State = "discarded"
		record.Error = "could not prove post-validation inputs: " + postErr.Error()
	} else if postProof.Digest != proof.Digest {
		record.State = "discarded"
		record.Error = "declared input proof changed while validation ran"
	} else {
		record.ObservedWorkspaceID = postWorkspace.ID
	}
	record.CompletedAt = time.Now()
	record.Duration = time.Since(started)
	_ = publishRecord(store, repoRoot, config.Digest, record)
	return record
}

func publishRecord(store *stateStore, repoRoot, configDigest string, record CheckRecord) error {
	return store.update(repoRoot, configDigest, func(checks map[string]CheckRecord) {
		checks[record.Name] = record
	})
}

func populateRecordProof(record *CheckRecord, proof CheckProof) {
	record.ScopeProof = proof.Digest
	record.InputProof = proof.InputDigest
	record.EnvironmentProof = proof.EnvironmentDigest
	record.ExecutablePath = proof.ExecutablePath
	record.ExecutableProof = proof.ExecutableDigest
	record.ObservedWorkspaceID = proof.ObservedWorkspaceID
	record.MatchedFiles = proof.MatchedFiles
	record.MatchedBytes = proof.MatchedBytes
}
