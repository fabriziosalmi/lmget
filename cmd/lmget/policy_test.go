package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fabriziosalmi/lmget"
)

// policyEnv points lmget at a fake Hugging Face, with nothing of this
// machine, and returns a project folder.
func policyEnv(t *testing.T, hf *mcpHF) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("LMGET_HOME", filepath.Join(tmp, "store"))
	t.Setenv("HF_HUB_CACHE", filepath.Join(tmp, "hfcache"))
	t.Setenv("HF_HOME", filepath.Join(tmp, "hf"))
	t.Setenv("HF_TOKEN", "")
	t.Setenv("HF_TOKEN_PATH", filepath.Join(tmp, "no-token"))
	t.Setenv("LMGET_HISTORY", "off")
	t.Setenv("LMGET_LICENSES", "")
	t.Setenv("LMGET_INDEX_URL", "https://127.0.0.1:9/index.json")
	srv := httptest.NewServer(hf.handler())
	t.Cleanup(srv.Close)
	t.Setenv("HF_ENDPOINT", srv.URL)
	return tmp
}

func TestGetAndSyncHonourTheLockPolicy(t *testing.T) {
	w := tinySafetensors()
	hf := &mcpHF{files: map[string][]byte{"model.safetensors": w}} // apache-2.0
	dir := policyEnv(t, hf)
	lock := filepath.Join(dir, "lmget.lock")
	s := sha256.Sum256(w)
	entry := fmt.Sprintf(`
[[model]]
ref = "hf:org/m"
commit = "%s"
to = "none"
license = "apache-2.0"

[[model.file]]
path = "model.safetensors"
size = %d
sha256 = "%s"
`, mcpCommit, len(w), hex.EncodeToString(s[:]))
	write := func(policy string) {
		os.WriteFile(lock, []byte("version = 1\n"+policy+entry), 0o644)
	}
	f := flags{to: "none", lockfile: lock, plain: true, conns: 2}

	write("\n[policy]\nlicenses = [\"mit\"]\n")
	if code := doPull(f, "org/m", false); code != lmget.ExitVerify {
		t.Fatalf("get under a policy without apache-2.0: exit %d", code)
	}
	if code := doSync(f, nil); code != lmget.ExitVerify {
		t.Fatalf("sync under a policy without apache-2.0: exit %d", code)
	}
	if hf.served.Load() != 0 {
		t.Fatal("a refused model was downloaded")
	}
	write("\n[policy]\nlicenses = [\"apache-2.0\"]\n")
	if code := doSync(f, nil); code != lmget.ExitOK {
		t.Fatalf("sync under a policy that allows it: exit %d", code)
	}
	b, _ := os.ReadFile(lock)
	if !strings.Contains(string(b), "[policy]") {
		t.Fatal("sync rewrote the lock without its policy")
	}
}

func TestMCPHonoursTheMachinePolicy(t *testing.T) {
	hf := &mcpHF{files: map[string][]byte{"model.safetensors": tinySafetensors()}}
	s, _ := startMCP(t, hf)
	t.Setenv("LMGET_LICENSES", "mit")
	text, isErr := toolText(t, s.call(1, "tools/call", map[string]any{"name": "get", "arguments": map[string]any{"model": "org/m", "confirm": true}}))
	if !isErr || !strings.Contains(text, "license policy") || hf.served.Load() != 0 {
		t.Fatalf("an agent got a model the machine's policy refuses: %v %s served=%d", isErr, text, hf.served.Load())
	}
}
