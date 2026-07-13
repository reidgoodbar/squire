package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGuestSquireSessionCommandCodexPreloadFirst(t *testing.T) {
	tmp := t.TempDir()
	preload := filepath.Join(tmp, "squire-preload.so")
	if err := os.WriteFile(preload, []byte("lib"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQUIRE_VM_GUEST_PRELOAD_LIB", preload)

	got := guestSquireSessionCommand("/usr/local/bin/squire", []string{"codex", "exec", "task"})
	want := []string{"/usr/local/bin/squire", "session", "--quiet", "--preload", "--", "codex", "exec", "task"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestGuestSquireSessionCommandCodexUsesAutoWithoutPreload(t *testing.T) {
	t.Setenv("SQUIRE_VM_GUEST_PRELOAD_LIB", filepath.Join(t.TempDir(), "missing.so"))

	got := guestSquireSessionCommand("/usr/local/bin/squire", []string{"/usr/local/bin/codex"})
	want := []string{"/usr/local/bin/squire", "session", "--quiet", "--", "/usr/local/bin/codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestGuestSquireSessionCommandTransportOverride(t *testing.T) {
	tmp := t.TempDir()
	preload := filepath.Join(tmp, "squire-preload.so")
	if err := os.WriteFile(preload, []byte("lib"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SQUIRE_VM_GUEST_PRELOAD_LIB", preload)
	t.Setenv("SQUIRE_VM_GUEST_SESSION_TRANSPORT", "preload")
	got := guestSquireSessionCommand("/squire", []string{"codex"})
	want := []string{"/squire", "session", "--quiet", "--preload", "--", "codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preload override command = %#v, want %#v", got, want)
	}

	t.Setenv("SQUIRE_VM_GUEST_SESSION_TRANSPORT", "path-shims")
	got = guestSquireSessionCommand("/squire", []string{"codex"})
	want = []string{"/squire", "session", "--quiet", "--preload", "--", "codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("removed path-shim override should fall back to preload when available: %#v", got)
	}
}

func TestGuestSquireSessionCommandNonCodexUsesSessionAuto(t *testing.T) {
	got := guestSquireSessionCommand("/squire", []string{"/bin/sh", "-c", "git rev-parse HEAD"})
	want := []string{"/squire", "session", "--quiet", "--", "/bin/sh", "-c", "git rev-parse HEAD"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestGuestCommandEnvAllowsOnlySquireDiagnostics(t *testing.T) {
	req := guestRequest{
		StoreRoot: "/store",
		Env: map[string]string{
			"SQUIRE_VM_GUEST_SESSION_TRANSPORT": "preload",
			"SQUIRE_PRELOAD_TRACE":              "1",
			"LD_PRELOAD":                        "/tmp/evil.so",
			"PATH":                              "/tmp/bin",
			"SECRET":                            "value",
		},
	}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("SQUIRE_PRELOAD_TRACE", "0")
	got := guestCommandEnv(req)
	env := envMap(got)
	if env["SQUIRE_STORE_ROOT"] != "/store" {
		t.Fatalf("store root not set: %#v", env)
	}
	if env["SQUIRE_VM_GUEST_SESSION_TRANSPORT"] != "preload" {
		t.Fatalf("guest transport not forwarded: %#v", env)
	}
	if env["SQUIRE_PRELOAD_TRACE"] != "1" {
		t.Fatalf("trace not overridden: %#v", env)
	}
	if env["PATH"] != "/usr/bin" {
		t.Fatalf("PATH should not be overridden: %#v", env)
	}
}

func TestGuestEnvAllowed(t *testing.T) {
	for _, key := range []string{"SQUIRE_VM_GUEST_SESSION_TRANSPORT", "SQUIRE_PRELOAD_TRACE", "SQUIRE_SHIM_REQUIRE_HIT"} {
		if !guestEnvAllowed(key) {
			t.Fatalf("%s should be allowed", key)
		}
	}
	for _, key := range []string{"SQUIRE_PRELOAD_ENABLE", "SQUIRE_PRELOAD_LIB", "SQUIRE_PRELOAD_HELPER"} {
		if !guestEnvAllowed(key) {
			t.Fatalf("%s should be allowed", key)
		}
	}
	for _, key := range []string{"LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "PATH", "SECRET"} {
		if guestEnvAllowed(key) {
			t.Fatalf("%s should not be allowed", key)
		}
	}
}

func envMap(entries []string) map[string]string {
	out := map[string]string{}
	for _, entry := range entries {
		for i := 0; i < len(entry); i++ {
			if entry[i] == '=' {
				out[entry[:i]] = entry[i+1:]
				break
			}
		}
	}
	return out
}
