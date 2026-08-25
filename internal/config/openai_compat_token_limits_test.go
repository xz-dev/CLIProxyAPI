package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAICompatibilityTokenLimitsConfigDecoding(t *testing.T) {
	const yamlConfig = `openai-compatibility:
  - models:
      - name: compat-upstream
        alias: compat-alias
        max-input-tokens: 1000000
        max-output-tokens: 48576
`
	const jsonConfig = `{"openai-compatibility":[{"models":[{"name":"compat-upstream","alias":"compat-alias","max-input-tokens":1000000,"max-output-tokens":48576}]}]}`

	for _, testCase := range []struct {
		name   string
		decode func(*Config) error
	}{
		{name: "YAML", decode: func(cfg *Config) error { return yaml.Unmarshal([]byte(yamlConfig), cfg) }},
		{name: "JSON", decode: func(cfg *Config) error { return json.Unmarshal([]byte(jsonConfig), cfg) }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var cfg Config
			if err := testCase.decode(&cfg); err != nil {
				t.Fatal(err)
			}
			model := cfg.OpenAICompatibility[0].Models[0]
			if model.MaxInputTokens != 1000000 || model.MaxOutputTokens != 48576 {
				t.Fatalf("token limits=%d/%d", model.MaxInputTokens, model.MaxOutputTokens)
			}
		})
	}
}

func TestOpenAICompatibilityTokenLimitsSurviveConfigSave(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	const original = `# provider catalog
openai-compatibility:
  - name: compat
    base-url: https://example.com/v1
    models:
      - name: compat-upstream
        alias: compat-alias
        max-input-tokens: 1000000
        max-output-tokens: 48576
`
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveConfigPreserveComments(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "# provider catalog") {
		t.Fatalf("saved config lost comment:\n%s", saved)
	}
	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	model := reloaded.OpenAICompatibility[0].Models[0]
	if model.MaxInputTokens != 1000000 || model.MaxOutputTokens != 48576 {
		t.Fatalf("saved token limits=%d/%d", model.MaxInputTokens, model.MaxOutputTokens)
	}
}
