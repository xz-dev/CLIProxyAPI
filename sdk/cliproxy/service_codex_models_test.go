package cliproxy

import (
	"context"
	"fmt"
	"maps"
	"testing"

	internalcodexmodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/models"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"gopkg.in/yaml.v3"
)

func TestRegisterModelsForAuthCodexOAuthContextProfile(t *testing.T) {
	const authID = "codex-oauth-context-profile"
	const modelID = "gpt-5.6-sol"

	var cfg config.Config
	if err := yaml.Unmarshal([]byte(`oauth-model-alias:
  codex:
    - name: gpt-5.6-sol
      alias: gpt-5.6-sol
      max-context-length: 372000
`), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	cfg.SanitizeOAuthModelAlias()

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		FileName: "codex-pro.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			"plan_type":                     "pro",
		},
	}
	service := &Service{cfg: &cfg}
	service.registerModelsForAuth(context.Background(), auth)

	models := modelRegistry.GetModelsForClient(authID)
	wantIDs := codexModelIDSet(internalregistry.GetCodexProModels())
	if gotIDs := codexModelIDSet(models); !maps.Equal(gotIDs, wantIDs) {
		t.Fatalf("registered model IDs changed: got %#v, want %#v", gotIDs, wantIDs)
	}

	var target *internalregistry.ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == modelID {
			target = model
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s registration count = %d, want 1", modelID, count)
	}
	if auth.Provider != "codex" || auth.Attributes["plan_type"] != "pro" {
		t.Fatalf("auth ownership changed: provider=%q plan=%q", auth.Provider, auth.Attributes["plan_type"])
	}
	if target.ContextLength != 921000 {
		t.Fatalf("static ContextLength = %d, want 921000", target.ContextLength)
	}
	if target.MaxContextLength != 372000 {
		t.Fatalf("MaxContextLength = %d, want 372000", target.MaxContextLength)
	}
	if target.MaxCompletionTokens != 128000 {
		t.Fatalf("MaxCompletionTokens = %d, want 128000", target.MaxCompletionTokens)
	}

	var catalogModel map[string]any
	for _, model := range modelRegistry.GetAvailableModels("openai") {
		if model["id"] == modelID {
			catalogModel = model
			break
		}
	}
	if catalogModel == nil {
		t.Fatalf("openai registry catalog is missing %s", modelID)
	}
	if got := catalogModel["context_length"]; got != 921000 {
		t.Fatalf("registry context_length = %#v, want 921000", got)
	}
	if got := catalogModel["max_context_length"]; got != 372000 {
		t.Fatalf("registry max_context_length = %#v, want 372000", got)
	}

	response := internalcodexmodels.BuildResponse([]map[string]any{catalogModel}, modelRegistry.GetModelProviders, false)
	detailed := response["models"].([]map[string]any)
	if len(detailed) != 1 {
		t.Fatalf("detailed catalog models = %d, want 1", len(detailed))
	}
	if got := detailed[0]["context_window"]; got != 372000 {
		t.Fatalf("context_window = %#v, want 372000", got)
	}
	if got := detailed[0]["max_context_window"]; got != 372000 {
		t.Fatalf("max_context_window = %#v, want 372000", got)
	}
	if got := detailed[0]["max_tokens"]; got != 128000 {
		t.Fatalf("max_tokens = %#v, want 128000", got)
	}
	if _, exists := detailed[0]["max_input_tokens"]; exists {
		t.Fatal("detailed catalog unexpectedly advertises max_input_tokens")
	}
	if got := detailed[0]["auto_compact_token_limit"]; got != nil {
		t.Fatalf("auto_compact_token_limit = %#v, want nil", got)
	}
}

func TestRegisterModelsForAuthCodexOAuthContextProfileEligibility(t *testing.T) {
	const modelID = "gpt-5.6-sol"

	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"codex": {{
			Name:             modelID,
			Alias:            modelID,
			MaxContextLength: 372000,
		}},
	}}
	cfg.SanitizeOAuthModelAlias()

	tests := []struct {
		name         string
		status       coreauth.Status
		fileName     string
		attributes   map[string]string
		wantModels   func() []*internalregistry.ModelInfo
		wantOverride bool
	}{
		{
			name:   "watcher file source without file name",
			status: coreauth.StatusActive,
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
				"plan_type":                     "pro",
			},
			wantModels:   internalregistry.GetCodexProModels,
			wantOverride: true,
		},
		{
			name:     "plus plan",
			status:   coreauth.StatusActive,
			fileName: "codex-plus.json",
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
				"plan_type":                     "plus",
			},
			wantModels: internalregistry.GetCodexPlusModels,
		},
		{
			name:     "team plan",
			status:   coreauth.StatusActive,
			fileName: "codex-team.json",
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
				"plan_type":                     "team",
			},
			wantModels: internalregistry.GetCodexTeamModels,
		},
		{
			name:     "inactive pro",
			status:   coreauth.StatusError,
			fileName: "codex-pro-error.json",
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
				"plan_type":                     "pro",
			},
			wantModels: internalregistry.GetCodexProModels,
		},
		{
			name:   "no file backing",
			status: coreauth.StatusActive,
			attributes: map[string]string{
				coreauth.AttributeAuthKind: coreauth.AuthKindOAuth,
				"plan_type":                "pro",
			},
			wantModels: internalregistry.GetCodexProModels,
		},
		{
			name:     "config source with file name",
			status:   coreauth.StatusActive,
			fileName: "not-file-backed.json",
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceConfig,
				"plan_type":                     "pro",
			},
			wantModels: internalregistry.GetCodexProModels,
		},
		{
			name:     "missing plan",
			status:   coreauth.StatusActive,
			fileName: "codex-unknown-plan.json",
			attributes: map[string]string{
				coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
				coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			},
			wantModels: internalregistry.GetCodexProModels,
		},
	}

	for index := range tests {
		testCase := tests[index]
		t.Run(testCase.name, func(t *testing.T) {
			authID := fmt.Sprintf("codex-oauth-context-profile-eligibility-%d", index)
			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(authID)
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			auth := &coreauth.Auth{
				ID:         authID,
				Provider:   "codex",
				FileName:   testCase.fileName,
				Status:     testCase.status,
				Attributes: testCase.attributes,
			}
			service := &Service{cfg: cfg}
			service.registerModelsForAuth(context.Background(), auth)

			gotModels := modelRegistry.GetModelsForClient(authID)
			wantModels := testCase.wantModels()
			if gotIDs, wantIDs := codexModelIDSet(gotModels), codexModelIDSet(wantModels); !maps.Equal(gotIDs, wantIDs) {
				t.Fatalf("registered model IDs changed: got %#v, want %#v", gotIDs, wantIDs)
			}

			var gotTarget, wantTarget *internalregistry.ModelInfo
			for _, model := range gotModels {
				if model != nil && model.ID == modelID {
					gotTarget = model
					break
				}
			}
			for _, model := range wantModels {
				if model != nil && model.ID == modelID {
					wantTarget = model
					break
				}
			}
			if gotTarget == nil || wantTarget == nil {
				t.Fatalf("missing %s: got=%v want=%v", modelID, gotTarget != nil, wantTarget != nil)
			}
			if gotTarget.ContextLength != wantTarget.ContextLength {
				t.Fatalf("ContextLength = %d, want unchanged %d", gotTarget.ContextLength, wantTarget.ContextLength)
			}
			wantMaxContextLength := wantTarget.MaxContextLength
			if testCase.wantOverride {
				wantMaxContextLength = 372000
			}
			if gotTarget.MaxContextLength != wantMaxContextLength {
				t.Fatalf("MaxContextLength = %d, want %d", gotTarget.MaxContextLength, wantMaxContextLength)
			}
			if gotTarget.MaxCompletionTokens != wantTarget.MaxCompletionTokens {
				t.Fatalf("MaxCompletionTokens = %d, want unchanged %d", gotTarget.MaxCompletionTokens, wantTarget.MaxCompletionTokens)
			}
		})
	}
}

func TestRegisterModelsForAuthCodexOAuthContextProfileDisabled(t *testing.T) {
	const authID = "codex-oauth-context-profile-disabled"
	const modelID = "gpt-5.6-sol"

	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"codex": {{Name: modelID, Alias: modelID, MaxContextLength: 372000}},
	}}
	cfg.SanitizeOAuthModelAlias()

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "codex",
		FileName: "codex-disabled.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			"plan_type":                     "pro",
		},
	}
	service := &Service{cfg: cfg}
	service.registerModelsForAuth(context.Background(), auth)

	registered := false
	for _, model := range modelRegistry.GetModelsForClient(authID) {
		if model != nil && model.ID == modelID && model.MaxContextLength == 372000 {
			registered = true
			break
		}
	}
	if !registered {
		t.Fatalf("active credential did not register %s with MaxContextLength 372000", modelID)
	}

	auth.Disabled = true
	service.registerModelsForAuth(context.Background(), auth)
	if models := modelRegistry.GetModelsForClient(authID); len(models) != 0 {
		t.Fatalf("disabled active-status credential retained %d registered models", len(models))
	}
}

func TestRegisterModelsForAuthOAuthContextProfileSkipsUnrelatedProvider(t *testing.T) {
	const authID = "claude-oauth-context-profile-ineligible"

	wantModels := internalregistry.GetClaudeModels()
	if len(wantModels) == 0 || wantModels[0] == nil {
		t.Fatal("expected at least one Claude model")
	}
	modelID := wantModels[0].ID
	cfg := &config.Config{OAuthModelAlias: map[string][]config.OAuthModelAlias{
		"claude": {{Name: modelID, Alias: modelID, MaxContextLength: 372000}},
	}}
	cfg.SanitizeOAuthModelAlias()

	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		FileName: "claude-pro.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			"plan_type":                     "pro",
		},
	}
	service := &Service{cfg: cfg}
	service.registerModelsForAuth(context.Background(), auth)

	gotModels := modelRegistry.GetModelsForClient(authID)
	if gotIDs, wantIDs := codexModelIDSet(gotModels), codexModelIDSet(wantModels); !maps.Equal(gotIDs, wantIDs) {
		t.Fatalf("registered model IDs changed: got %#v, want %#v", gotIDs, wantIDs)
	}

	var gotTarget *internalregistry.ModelInfo
	for _, model := range gotModels {
		if model != nil && model.ID == modelID {
			gotTarget = model
			break
		}
	}
	if gotTarget == nil {
		t.Fatalf("missing registered Claude model %q", modelID)
	}
	if gotTarget.ContextLength != wantModels[0].ContextLength {
		t.Fatalf("ContextLength = %d, want unchanged %d", gotTarget.ContextLength, wantModels[0].ContextLength)
	}
	if gotTarget.MaxContextLength != wantModels[0].MaxContextLength {
		t.Fatalf("MaxContextLength = %d, want unchanged %d", gotTarget.MaxContextLength, wantModels[0].MaxContextLength)
	}
	if gotTarget.MaxCompletionTokens != wantModels[0].MaxCompletionTokens {
		t.Fatalf("MaxCompletionTokens = %d, want unchanged %d", gotTarget.MaxCompletionTokens, wantModels[0].MaxCompletionTokens)
	}
}

func TestEligibleForCodexOAuthModelMetadataRejectsDisabledAndUnrelatedProvider(t *testing.T) {
	auth := &coreauth.Auth{
		Provider: "codex",
		FileName: "codex-pro.json",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			coreauth.AttributeAuthKind:      coreauth.AuthKindOAuth,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceFile,
			"plan_type":                     "pro",
		},
	}
	if !eligibleForCodexOAuthModelMetadata(auth, "codex", coreauth.AuthKindOAuth) {
		t.Fatal("eligible active file-backed Codex OAuth Pro credential was rejected")
	}

	disabled := *auth
	disabled.Disabled = true
	if eligibleForCodexOAuthModelMetadata(&disabled, "codex", coreauth.AuthKindOAuth) {
		t.Fatal("disabled active-status credential was eligible")
	}
	if eligibleForCodexOAuthModelMetadata(auth, "claude", coreauth.AuthKindOAuth) {
		t.Fatal("unrelated provider credential was eligible")
	}
}

func TestRegisterModelsForAuthCodexAPIKeyModels(t *testing.T) {
	defaultModels := internalregistry.GetCodexProModels()
	if len(defaultModels) == 0 {
		t.Fatal("expected Codex Pro default models")
	}

	excludedModelID := defaultModels[0].ID
	tests := []struct {
		name        string
		entry       config.CodexKey
		wantIDs     map[string]struct{}
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "defaults without explicit models",
			entry:       config.CodexKey{APIKey: "default-key"},
			wantIDs:     codexModelIDSet(defaultModels),
			wantPresent: []string{"gpt-image-1.5", "gpt-image-2"},
		},
		{
			name: "only explicitly configured models",
			entry: config.CodexKey{
				APIKey: "configured-key",
				Models: []internalconfig.CodexModel{{
					Name: "upstream-codex", Alias: "configured-codex",
				}},
			},
			wantIDs:    map[string]struct{}{"configured-codex": {}},
			wantAbsent: []string{"gpt-image-1.5", "gpt-image-2"},
		},
		{
			name: "exclusions apply to defaults",
			entry: config.CodexKey{
				APIKey:         "excluded-key",
				ExcludedModels: []string{excludedModelID},
			},
			wantIDs: codexModelIDSet(defaultModels[1:]),
		},
	}

	for index := range tests {
		testCase := tests[index]
		t.Run(testCase.name, func(t *testing.T) {
			authID := fmt.Sprintf("codex-api-key-models-%d", index)
			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(authID)
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			service := &Service{cfg: &config.Config{CodexKey: []config.CodexKey{testCase.entry}}}
			auth := &coreauth.Auth{
				ID:       authID,
				Provider: "codex",
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					coreauth.AttributeAPIKey:      testCase.entry.APIKey,
					coreauth.AttributeConfigIndex: "0",
					coreauth.AttributeSource:      "config:codex:test",
				},
			}

			service.registerModelsForAuth(context.Background(), auth)
			gotIDs := codexModelIDSet(modelRegistry.GetModelsForClient(authID))
			if len(gotIDs) != len(testCase.wantIDs) {
				t.Fatalf("registered model IDs = %#v, want %#v", gotIDs, testCase.wantIDs)
			}
			for modelID := range testCase.wantIDs {
				if _, ok := gotIDs[modelID]; !ok {
					t.Errorf("missing registered model %q", modelID)
				}
			}
			for _, modelID := range testCase.wantPresent {
				if _, ok := gotIDs[modelID]; !ok {
					t.Errorf("missing required registered model %q", modelID)
				}
			}
			for _, modelID := range testCase.wantAbsent {
				if _, ok := gotIDs[modelID]; ok {
					t.Errorf("unexpected registered model %q", modelID)
				}
			}
		})
	}
}

func TestRegisterModelsForAuthCodexAPIKeyDefaultRequiresConfigMatch(t *testing.T) {
	defaultIDs := codexModelIDSet(internalregistry.GetCodexProModels())
	tests := []struct {
		name       string
		config     config.Config
		attributes map[string]string
		wantIDs    map[string]struct{}
	}{
		{
			name: "valid index with unmatched API key",
			config: config.Config{CodexKey: []config.CodexKey{{
				APIKey: "configured-key",
			}}},
			attributes: map[string]string{
				coreauth.AttributeAPIKey:      "stale-key",
				coreauth.AttributeConfigIndex: "0",
				coreauth.AttributeSource:      "config:codex:stale",
			},
			wantIDs: map[string]struct{}{},
		},
		{
			name: "valid index with unmatched base URL",
			config: config.Config{CodexKey: []config.CodexKey{{
				APIKey: "configured-key", BaseURL: "https://new.example.com",
			}}},
			attributes: map[string]string{
				coreauth.AttributeAPIKey:      "configured-key",
				coreauth.AttributeConfigIndex: "0",
				coreauth.AttributeSource:      "config:codex:stale",
				"base_url":                    "https://old.example.com",
			},
			wantIDs: map[string]struct{}{},
		},
		{
			name: "stale index falls back to matching credentials",
			config: config.Config{CodexKey: []config.CodexKey{
				{
					APIKey: "wrong-key",
					Models: []internalconfig.CodexModel{{Name: "wrong-model"}},
				},
				{APIKey: "configured-key"},
			}},
			attributes: map[string]string{
				coreauth.AttributeAPIKey:      "configured-key",
				coreauth.AttributeConfigIndex: "0",
				coreauth.AttributeSource:      "config:codex:stale",
			},
			wantIDs: defaultIDs,
		},
		{
			name: "API key ignores OAuth plan type",
			config: config.Config{CodexKey: []config.CodexKey{{
				APIKey: "configured-key",
			}}},
			attributes: map[string]string{
				coreauth.AttributeAPIKey:      "configured-key",
				coreauth.AttributeConfigIndex: "0",
				coreauth.AttributeSource:      "config:codex:test",
				"plan_type":                   "free",
			},
			wantIDs: defaultIDs,
		},
	}

	for index := range tests {
		testCase := tests[index]
		t.Run(testCase.name, func(t *testing.T) {
			authID := fmt.Sprintf("codex-api-key-config-match-%d", index)
			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(authID)
			modelRegistry.RegisterClient(authID, "codex", []*internalregistry.ModelInfo{{ID: "stale-model"}})
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			service := &Service{cfg: &testCase.config}
			auth := &coreauth.Auth{
				ID:         authID,
				Provider:   "codex",
				Status:     coreauth.StatusActive,
				Attributes: testCase.attributes,
			}

			service.registerModelsForAuth(context.Background(), auth)
			gotIDs := codexModelIDSet(modelRegistry.GetModelsForClient(authID))
			if len(gotIDs) != len(testCase.wantIDs) {
				t.Fatalf("registered model IDs = %#v, want %#v", gotIDs, testCase.wantIDs)
			}
			for modelID := range testCase.wantIDs {
				if _, ok := gotIDs[modelID]; !ok {
					t.Errorf("missing registered model %q", modelID)
				}
			}
		})
	}
}

func TestRegisterConfigAPIKeyAuthsCodexModelModes(t *testing.T) {
	defaultIDs := codexModelIDSet(internalregistry.GetCodexProModels())
	tests := []struct {
		name       string
		models     []internalconfig.CodexModel
		wantIDs    map[string]struct{}
		wantImages bool
	}{
		{
			name:       "empty models uses defaults with images",
			wantIDs:    defaultIDs,
			wantImages: true,
		},
		{
			name: "configured models replace defaults",
			models: []internalconfig.CodexModel{{
				Name: "runtime-upstream", Alias: "runtime-configured",
			}},
			wantIDs: map[string]struct{}{"runtime-configured": {}},
		},
	}

	for index := range tests {
		testCase := tests[index]
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &config.Config{CodexKey: []config.CodexKey{{
				APIKey: fmt.Sprintf("runtime-key-%d", index),
				Models: testCase.models,
			}}}
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: cfg, coreManager: manager}
			service.registerConfigAPIKeyAuths(context.Background(), cfg)

			auths := manager.List()
			modelRegistry := internalregistry.GetGlobalRegistry()
			for _, auth := range auths {
				if auth != nil {
					authID := auth.ID
					t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
				}
			}
			if len(auths) != 1 {
				t.Fatalf("runtime auth count = %d, want 1", len(auths))
			}

			registeredIDs := codexModelIDSet(modelRegistry.GetModelsForClient(auths[0].ID))
			if len(registeredIDs) != len(testCase.wantIDs) {
				t.Fatalf("registered model IDs = %#v, want %#v", registeredIDs, testCase.wantIDs)
			}
			for modelID := range testCase.wantIDs {
				if _, ok := registeredIDs[modelID]; !ok {
					t.Errorf("missing registered model %q", modelID)
				}
			}
			for _, modelID := range []string{"gpt-image-1.5", "gpt-image-2"} {
				_, registered := registeredIDs[modelID]
				if registered != testCase.wantImages {
					t.Errorf("registered model %q = %t, want %t", modelID, registered, testCase.wantImages)
				}
				if testCase.wantImages {
					if _, available := openAIModelIDSet(modelRegistry.GetAvailableModels("openai"))[modelID]; !available {
						t.Errorf("/v1/models source is missing %q", modelID)
					}
				}
			}
		})
	}
}

func codexModelIDSet(models []*internalregistry.ModelInfo) map[string]struct{} {
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil && model.ID != "" {
			ids[model.ID] = struct{}{}
		}
	}
	return ids
}

func openAIModelIDSet(models []map[string]any) map[string]struct{} {
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if modelID, ok := model["id"].(string); ok && modelID != "" {
			ids[modelID] = struct{}{}
		}
	}
	return ids
}
