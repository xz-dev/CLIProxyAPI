package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClaudeRichMetadataConfigRoundTrip(t *testing.T) {
	const payload = `claude-api-key:
  - api-key: test
    models:
      - name: claude-upstream
        alias: claude-alias
        max-context-length: 200000
        max-input-tokens: 180000
        max-output-tokens: 64000
        input-modalities: [text, image]
        output-modalities: [text]
`
	for _, testCase := range []struct {
		name   string
		decode func([]byte, *Config) error
	}{
		{name: "yaml", decode: func(data []byte, cfg *Config) error { return yaml.Unmarshal(data, cfg) }},
		{name: "json", decode: func(_ []byte, cfg *Config) error {
			return json.Unmarshal([]byte(`{"claude-api-key":[{"api-key":"test","models":[{"name":"claude-upstream","alias":"claude-alias","max-context-length":200000,"max-input-tokens":180000,"max-output-tokens":64000,"input-modalities":["text","image"],"output-modalities":["text"]}]}]}`), cfg)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var cfg Config
			if err := testCase.decode([]byte(payload), &cfg); err != nil {
				t.Fatal(err)
			}
			model := cfg.ClaudeKey[0].Models[0]
			if model.MaxContextLength != 200000 || model.MaxInputTokens != 180000 || model.MaxOutputTokens != 64000 {
				t.Fatalf("limits=%+v", model)
			}
			if len(model.InputModalities) != 2 || len(model.OutputModalities) != 1 {
				t.Fatalf("modalities=%+v/%+v", model.InputModalities, model.OutputModalities)
			}
		})
	}
}
