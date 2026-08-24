package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestBuildOpenAICompatibilityConfigModelsPropagatesTokenLimits(t *testing.T) {
	models := buildOpenAICompatibilityConfigModels(&config.OpenAICompatibility{
		Name: "openrouter",
		Models: []config.OpenAICompatibilityModel{{
			Name:            "upstream-model",
			Alias:           "public-model",
			MaxInputTokens:  1000000,
			MaxOutputTokens: 48576,
		}},
	})
	if len(models) != 1 {
		t.Fatalf("models=%v", models)
	}
	model := models[0]
	if model.InputTokenLimit != 1000000 || model.OutputTokenLimit != 48576 || model.MaxCompletionTokens != 48576 {
		t.Fatalf("token limits=%d/%d/%d", model.InputTokenLimit, model.OutputTokenLimit, model.MaxCompletionTokens)
	}
}
