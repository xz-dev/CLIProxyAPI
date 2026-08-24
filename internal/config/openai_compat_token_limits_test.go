package config

import (
	"encoding/json"
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
