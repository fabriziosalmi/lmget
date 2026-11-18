package lmget

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLicensePolicy(t *testing.T) {
	f := &fakeHF{files: modelFiles()} // apache-2.0
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	c := newTestClient(t, srv, t.TempDir())
	pull := func(machine, project []string, dry bool) error {
		c.opt.Licenses = machine
		_, err := c.Pull(context.Background(), Request{Ref: hfRef("org/m"), Target: Target{Kind: "none"}, Licenses: project, DryRun: dry}, nil)
		return err
	}
	for name, tc := range map[string]struct {
		machine, project []string
		ok               bool
	}{
		"no policy":                 {nil, nil, true},
		"machine allows":            {[]string{"mit", "apache-2.0"}, nil, true},
		"machine refuses":           {[]string{"mit"}, nil, false},
		"project refuses":           {nil, []string{"mit"}, false},
		"both allow":                {[]string{"apache-2.0"}, []string{"apache-2.0", "mit"}, true},
		"machine yes, project no":   {[]string{"apache-2.0"}, []string{"mit"}, false},
		"project yes, machine no":   {[]string{"mit"}, []string{"apache-2.0"}, false},
		"none is not every license": {[]string{"none"}, nil, false},
	} {
		for _, dry := range []bool{true, false} {
			err := pull(tc.machine, tc.project, dry)
			if tc.ok && err != nil {
				t.Errorf("%s (dry %v): %v", name, dry, err)
			}
			if !tc.ok {
				if e := AsError(err); e.Code != ExitVerify || e.Kind != "policy" {
					t.Errorf("%s (dry %v): want a policy refusal, got %v", name, dry, err)
				}
			}
		}
	}
	served := f.served.Load()
	f.noLicense.Store(true)
	c2 := newTestClient(t, srv, t.TempDir())
	c2.opt.Licenses = []string{"apache-2.0"}
	if _, err := c2.Pull(context.Background(), Request{Ref: hfRef("org/n"), Target: Target{Kind: "none"}}, nil); AsError(err).Kind != "policy" {
		t.Fatalf("a model with no license passed a policy without none: %v", err)
	}
	if f.served.Load() != served {
		t.Fatal("a refused model was downloaded")
	}
	c2.opt.Licenses = []string{"apache-2.0", "none"}
	if _, err := c2.Pull(context.Background(), Request{Ref: hfRef("org/n"), Target: Target{Kind: "none"}}, nil); err != nil {
		t.Fatalf("none allows a model with no license: %v", err)
	}
}

func TestParseLicenses(t *testing.T) {
	if got := strings.Join(ParseLicenses(" Apache-2.0, mit\tllama3.2,,none "), "|"); got != "apache-2.0|mit|llama3.2|none" {
		t.Fatal(got)
	}
	if ParseLicenses("") != nil {
		t.Fatal("an empty list is no policy")
	}
}

func TestLockPolicyIsKept(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "lmget.lock")
	sha := strings.Repeat("a", 64)
	body := `version = 1

[policy]
licenses = ["Apache-2.0", "mit"]

[[model]]
ref = "hf:org/m"
commit = "` + strings.Repeat("0", 40) + `"
to = "hf"
license = "apache-2.0"

[[model.file]]
path = "config.json"
size = 2
sha256 = "` + sha + `"
`
	os.WriteFile(p, []byte(body), 0o644)
	l, err := ReadLock(p)
	if err != nil {
		t.Fatal(err)
	}
	if l.Policy == nil || strings.Join(l.Policy.Licenses, ",") != "apache-2.0,mit" {
		t.Fatalf("policy %+v", l.Policy)
	}
	if err := l.Write(); err != nil {
		t.Fatal(err)
	}
	again, err := ReadLock(p)
	if err != nil || again.Policy == nil || strings.Join(again.Policy.Licenses, ",") != "apache-2.0,mit" {
		t.Fatalf("rewriting the lock lost the policy: %+v %v", again, err)
	}
	for _, bad := range []string{`licenses = ["a, b"]`, `licenses = [""]`, `licenses = ["x"]` + "\nother = 1"} {
		os.WriteFile(p, []byte(strings.Replace(body, `licenses = ["Apache-2.0", "mit"]`, bad, 1)), 0o644)
		if _, err := ReadLock(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestCycloneDX(t *testing.T) {
	sha := func(c string) string { return strings.Repeat(c, 64) }
	man := sha("b")
	l := &Lock{Version: 1, Models: []LockModel{
		{Ref: "hf:Org/Model-X", Commit: strings.Repeat("A", 40), License: "apache-2.0", Files: []LockFile{{Path: "model.safetensors", Size: 1, SHA256: sha("c")}}},
		{Ref: "hf:org/custom", Commit: strings.Repeat("1", 40), License: "llama3.2", Files: []LockFile{{Path: "a.gguf", Size: 1, SHA256: sha("d")}}},
		{Ref: "hf:org/bare", Commit: strings.Repeat("2", 40), License: LicenseNone, Files: []LockFile{{Path: "a.bin", Size: 1, SHA256: sha("e")}}},
		{Ref: "ollama:qwen3:0.6b", Commit: man, Files: []LockFile{{Path: OllamaManifestFile, Size: 1, SHA256: man}}},
		{Ref: "ollama:hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M", Commit: man, Files: []LockFile{{Path: OllamaManifestFile, Size: 1, SHA256: man}}},
		{Ref: "https://example.com/files/w.gguf?X-Amz-Signature=secretvalue", Commit: urlID("https://example.com/files/w.gguf?X-Amz-Signature=secretvalue"),
			Files: []LockFile{{Path: "w.gguf", Size: 1, SHA256: sha("f")}}},
	}}
	out, err := l.CycloneDX("v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := l.CycloneDX("v9.9.9"); string(again) != string(out) {
		t.Fatal("the same lock gave other bytes")
	}
	if strings.Contains(string(out), "secretvalue") {
		t.Fatal("a signed URL's query reached the bill of materials")
	}
	var b struct {
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		Components  []struct {
			Type     string `json:"type"`
			BOMRef   string `json:"bom-ref"`
			Group    string `json:"group"`
			Name     string `json:"name"`
			Version  string `json:"version"`
			PURL     string `json:"purl"`
			Licenses []struct {
				License struct{ ID, Name string } `json:"license"`
			} `json:"licenses"`
			Components []struct {
				Type   string                          `json:"type"`
				BOMRef string                          `json:"bom-ref"`
				Hashes []struct{ Alg, Content string } `json:"hashes"`
			} `json:"components"`
		} `json:"components"`
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatal(err)
	}
	if b.BOMFormat != "CycloneDX" || b.SpecVersion != "1.6" || len(b.Components) != 6 {
		t.Fatalf("%s %s %d components", b.BOMFormat, b.SpecVersion, len(b.Components))
	}
	refs := map[string]bool{}
	for _, c := range b.Components {
		for _, r := range append([]string{c.BOMRef}, func() (rs []string) {
			for _, f := range c.Components {
				rs = append(rs, f.BOMRef)
			}
			return
		}()...) {
			if refs[r] {
				t.Fatalf("bom-ref %q twice", r)
			}
			refs[r] = true
		}
		for _, f := range c.Components {
			if f.Type != "file" || len(f.Hashes) != 1 || f.Hashes[0].Alg != "SHA-256" || len(f.Hashes[0].Content) != 64 {
				t.Fatalf("file component %+v", f)
			}
		}
	}
	hf := b.Components[0]
	// purl: organisation and name keep their case, the commit is lowercased
	if hf.Type != "machine-learning-model" || hf.Group != "Org" || hf.Name != "Model-X" ||
		hf.PURL != "pkg:huggingface/Org/Model-X@"+strings.Repeat("a", 40) || hf.Licenses[0].License.ID != "Apache-2.0" {
		t.Fatalf("hf component %+v", hf)
	}
	if lic := b.Components[1].Licenses[0].License; lic.ID != "" || lic.Name != "llama3.2" {
		t.Fatalf("a license that is not SPDX is a name: %+v", lic)
	}
	if len(b.Components[2].Licenses) != 0 {
		t.Fatal("no license is no license entry")
	}
	if b.Components[4].Name != "hf.co/unsloth/Qwen3-0.6B-GGUF:Q4_K_M" || b.Components[5].Type != "file" || b.Components[5].Name != "w.gguf" {
		t.Fatalf("ollama and url components: %+v / %+v", b.Components[4], b.Components[5])
	}
}
