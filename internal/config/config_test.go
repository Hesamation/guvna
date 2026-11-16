package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
port: 20128
providers:
  - name: groq
    type: openai
    base_url: https://api.groq.com/openai
    key_env: GROQ_1_KEY
    models:
      - llama-3.3-70b-versatile
  - name: gemini
    type: gemini
    base_url: https://generativelanguage.googleapis.com
    key_env: GEMINI_1_KEY
    models:
      - gemini-2.5-flash
chains:
  - name: fast
    steps:
      - provider: groq
        model: llama-3.3-70b-versatile
      - provider: gemini
        model: gemini-2.5-flash
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 20128 {
		t.Errorf("port = %d, want 20128", cfg.Port)
	}
	if cfg.Bind != DefaultBind {
		t.Errorf("bind = %q, want default %q", cfg.Bind, DefaultBind)
	}
	if len(cfg.Providers) != 2 || len(cfg.Chains) != 1 {
		t.Fatalf("got %d providers, %d chains", len(cfg.Providers), len(cfg.Chains))
	}
}

func TestBindValidation(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bind = "0.0.0.0"
	if err := cfg.Validate(); err != nil {
		t.Errorf("explicit 0.0.0.0 rejected: %v", err)
	}
	cfg.Bind = "not-an-ip"
	if err := cfg.Validate(); err == nil {
		t.Errorf("invalid bind accepted")
	}
	cfg.Bind = ""
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Bind != DefaultBind {
		t.Errorf("bind = %q, want default %q after empty", cfg.Bind, DefaultBind)
	}
}

func TestLoadDefaultPort(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = 0
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("port = %d, want %d", cfg.Port, DefaultPort)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidateDuplicateProvider(t *testing.T) {
	dup := sample + "\n"
	cfg, err := Load(writeTemp(t, dup))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers = append(cfg.Providers, cfg.Providers[0])
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate provider error")
	}
}

func TestValidateUnknownChainProvider(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Chains[0].Steps[0].Provider = "nope"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown provider error")
	}
}

func TestValidateNoChainsOK(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Chains = nil
	if err := cfg.Validate(); err != nil {
		t.Fatalf("no-chains config must validate: %v", err)
	}
}

func TestValidateEmptyModelsOK(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers[0].Models = nil
	cfg.Providers[0].Prefixes = []string{"groq"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("models-less provider must validate: %v", err)
	}
}

func TestValidateEmptyStep(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Chains[0].Steps[0].Model = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected empty model error")
	}
}

func TestValidateUnknownProviderType(t *testing.T) {
	cfg, err := Load(writeTemp(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Providers[0].Type = "anthropic"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown type error")
	}
}
