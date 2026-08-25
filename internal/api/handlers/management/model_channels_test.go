package management

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type modelChannelTestStore struct{}

func (modelChannelTestStore) List(context.Context) ([]*coreauth.Auth, error)       { return nil, nil }
func (modelChannelTestStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", nil }
func (modelChannelTestStore) Delete(context.Context, string) error                 { return nil }

func newModelChannelTestHandler(t *testing.T, yamlConfig string, cfg *config.Config, auths ...*coreauth.Auth) (*Handler, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yamlConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(modelChannelTestStore{}, nil, nil)
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatal(err)
		}
	}
	return NewHandler(cfg, path, manager), path
}

func performModelChannelRequest(handler gin.HandlerFunc, method, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler(ctx)
	return recorder
}

func TestCanonicalizeModelChannelURL(t *testing.T) {
	got, err := canonicalizeModelChannelURL(" HTTPS://Example.COM:443/v1/?tenant=A ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/v1?tenant=A" {
		t.Fatalf("canonical URL = %q", got)
	}
	for _, invalid := range []string{"https://user:pass@example.com", "https://example.com/#fragment", "ftp://example.com"} {
		if _, err := canonicalizeModelChannelURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestGetModelChannelsSanitized(t *testing.T) {
	cfg := &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name: "demo", BaseURL: "https://EXAMPLE.com/v1/", Prefix: "demo",
			APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret-openai"}},
			Headers:       map[string]string{"X-Secret": "secret-header"},
			Models:        []config.OpenAICompatibilityModel{{Name: "upstream", Alias: "alias", MaxOutputTokens: 100}},
		}},
		ClaudeKey: []config.ClaudeKey{{
			APIKey: "secret-claude", BaseURL: "", ProxyURL: "http://claude-secret-proxy", Prefix: "native",
			Headers: map[string]string{"Cookie": "secret-cookie"}, Models: []config.ClaudeModel{{Name: "claude-upstream", Alias: "claude-alias", InputModalities: []string{"text"}}},
		}},
	}
	authOpenAI := &coreauth.Auth{ID: "secret-auth-id-openai", Provider: util.OpenAICompatibleProviderKey("demo"), Prefix: "demo", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "base_url": "https://EXAMPLE.com/v1/", "api_key": "secret-openai"}}
	authClaude := &coreauth.Auth{ID: "secret-auth-id-claude", Provider: "claude", Prefix: "native", Status: coreauth.StatusActive, ProxyURL: "http://claude-secret-proxy", Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "api_key": "secret-claude"}}
	h, _ := newModelChannelTestHandler(t, "{}\n", cfg, authOpenAI, authClaude)
	response := performModelChannelRequest(h.GetModelChannels, http.MethodGet, "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"secret-openai", "secret-claude", "secret-header", "secret-cookie", "secret-proxy", "secret-auth-id"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("descriptor leaked %q: %s", secret, response.Body.String())
		}
	}
	var payload struct {
		Channels []modelChannelDescriptor `json:"channels"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Channels) != 2 || !payload.Channels[0].Ready || !payload.Channels[1].Ready {
		t.Fatalf("channels=%+v", payload.Channels)
	}
	if payload.Channels[0].Selector.BaseURL != "https://example.com/v1" {
		t.Fatalf("selector=%+v", payload.Channels[0].Selector)
	}
}

func TestResolveOpenAIChannelDuplicateAndClaudeDrift(t *testing.T) {
	base := config.OpenAICompatibility{Name: "same", BaseURL: "https://example.com/v1"}
	h := NewHandlerWithoutConfigFilePath(&config.Config{OpenAICompatibility: []config.OpenAICompatibility{base, base}, ClaudeKey: []config.ClaudeKey{{BaseURL: "https://claude.example", Prefix: "one"}}}, nil)
	if _, _, _, err := h.resolveOpenAIChannelLocked(modelChannelSelector{Name: "same", BaseURL: "https://EXAMPLE.com:443/v1/"}); err == nil || err.(*modelChannelError).Status != http.StatusConflict {
		t.Fatalf("duplicate error=%v", err)
	}
	index := 0
	if _, _, _, err := h.resolveClaudeChannelLocked(modelChannelSelector{ConfigIndex: &index, BaseURL: "https://claude.example", Prefix: "changed"}); err == nil || err.(*modelChannelError).Status != http.StatusConflict {
		t.Fatalf("drift error=%v", err)
	}
}

type recordingCatalogTransport struct {
	request *http.Request
	body    []byte
	status  int
	err     error
	delay   time.Duration
}

func (transport *recordingCatalogTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.request = request.Clone(request.Context())
	if transport.delay > 0 {
		select {
		case <-time.After(transport.delay):
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
	}
	if transport.err != nil {
		return nil, transport.err
	}
	return &http.Response{StatusCode: transport.status, Body: io.NopCloser(strings.NewReader(string(transport.body))), Header: http.Header{}}, nil
}

func TestFetchModelChannelCatalogProfileAuthAndHeaders(t *testing.T) {
	cfg := &config.Config{ClaudeKey: []config.ClaudeKey{{APIKey: "live-key", BaseURL: "https://gateway.example/root/", Headers: map[string]string{"Authorization": "bad", "X-API-Key": "bad", "Anthropic-Version": "bad", "X-Custom": "ok"}}}}
	auth := &coreauth.Auth{ID: "claude", Provider: "claude", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "base_url": "https://gateway.example/root/", "api_key": "live-key"}}
	h, _ := newModelChannelTestHandler(t, "{}\n", cfg, auth)
	index := 0
	selector := modelChannelSelector{ConfigIndex: &index, BaseURL: "https://gateway.example/root", Prefix: ""}
	revision := claudeChannelRevision(&cfg.ClaudeKey[0], selector)
	transport := &recordingCatalogTransport{status: http.StatusOK, body: []byte(`{"data":[]}`)}
	oldClient := modelCatalogHTTPClient
	modelCatalogHTTPClient = func(http.RoundTripper) *http.Client {
		return &http.Client{Transport: transport, Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	defer func() { modelCatalogHTTPClient = oldClient }()
	body := `{"kind":"claude","selector":{"config_index":0,"base_url":"https://gateway.example/root","prefix":""},"expected_revision":"` + revision + `","profile":"claude_models","query":{"limit":100,"after_id":"a","before_id":"b"}}`
	response := performModelChannelRequest(h.FetchModelChannelCatalog, http.MethodPost, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var catalogResponse struct {
		StatusCode int    `json:"status_code"`
		Body       string `json:"body"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &catalogResponse); err != nil || catalogResponse.StatusCode != http.StatusOK || catalogResponse.Body != `{"data":[]}` {
		t.Fatalf("catalog response=%+v err=%v", catalogResponse, err)
	}
	if got := transport.request.URL.String(); got != "https://gateway.example/root/v1/models?after_id=a&before_id=b&limit=100" {
		t.Fatalf("URL=%q", got)
	}
	if got := transport.request.Header.Get("Authorization"); got != "Bearer live-key" {
		t.Fatalf("Authorization=%q", got)
	}
	if transport.request.Header.Get("X-Api-Key") != "" || transport.request.Header.Get("Anthropic-Version") != "2023-06-01" || transport.request.Header.Get("X-Custom") != "ok" {
		t.Fatalf("headers=%v", transport.request.Header)
	}
}

func TestCatalogTargetAndDynamicHeaders(t *testing.T) {
	for _, testCase := range []struct {
		base, profile, want string
	}{
		{base: "https://openai.example/v1", profile: "openai_models", want: "https://openai.example/v1/models"},
		{base: "https://openai.example/v1/models?tenant=a", profile: "openai_models", want: "https://openai.example/v1/models?tenant=a"},
		{base: "https://claude.example/v1/messages?tenant=a", profile: "claude_models", want: "https://claude.example/v1/models?tenant=a"},
	} {
		target, err := catalogTarget(testCase.base, testCase.profile)
		if err != nil || target.String() != testCase.want {
			t.Fatalf("catalogTarget(%q, %q)=%v, %v", testCase.base, testCase.profile, target, err)
		}
	}
	headers := http.Header{}
	copyConfiguredHeaders(headers, map[string]string{"X-Static": "value", "X-Dynamic": "$request.header.X-Secret"})
	if headers.Get("X-Static") != "value" || headers.Get("X-Dynamic") != "" {
		t.Fatalf("headers=%v", headers)
	}
}

func TestChannelRevisionDetectsSensitiveStateWithoutLeaking(t *testing.T) {
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	base := config.OpenAICompatibility{Name: "demo", BaseURL: "https://example.com/v1", Prefix: "p", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret-one", ProxyURL: "http://proxy-one"}}, Headers: map[string]string{"X-Secret": "header-one"}, Models: []config.OpenAICompatibilityModel{{Name: "one"}}}
	baseline := openAIChannelRevision(&base, selector)
	for name, mutate := range map[string]func(*config.OpenAICompatibility){
		"credential": func(entry *config.OpenAICompatibility) { entry.APIKeyEntries[0].APIKey = "secret-two" },
		"proxy":      func(entry *config.OpenAICompatibility) { entry.APIKeyEntries[0].ProxyURL = "http://proxy-two" },
		"header":     func(entry *config.OpenAICompatibility) { entry.Headers["X-Secret"] = "header-two" },
		"prefix":     func(entry *config.OpenAICompatibility) { entry.Prefix = "changed" },
		"model":      func(entry *config.OpenAICompatibility) { entry.Models[0].Name = "two" },
	} {
		changed := base
		changed.APIKeyEntries = append([]config.OpenAICompatibilityAPIKey(nil), base.APIKeyEntries...)
		changed.Headers = copyStringMap(base.Headers)
		changed.Models = append([]config.OpenAICompatibilityModel(nil), base.Models...)
		mutate(&changed)
		revision := openAIChannelRevision(&changed, selector)
		if revision == baseline {
			t.Fatalf("%s did not change revision", name)
		}
		for _, secret := range []string{"secret-one", "secret-two", "proxy-one", "proxy-two", "header-one", "header-two"} {
			if strings.Contains(revision, secret) {
				t.Fatalf("revision leaked %q", secret)
			}
		}
	}
}

func TestCatalogOpenAIAuthFollowsConfiguredCredentialOrder(t *testing.T) {
	auths := []*coreauth.Auth{
		{ID: "second", Provider: "demo", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "api_key": "key-two"}},
		{ID: "first", Provider: "demo", Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "api_key": "key-one"}},
	}
	entry := &config.OpenAICompatibility{Name: "demo", BaseURL: "https://example.com/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "key-one"}, {APIKey: "key-two"}}}
	for _, auth := range auths {
		auth.Provider = util.OpenAICompatibleProviderKey(entry.Name)
		auth.Attributes["base_url"] = entry.BaseURL
	}
	for _, order := range [][]*coreauth.Auth{auths, {auths[1], auths[0]}} {
		auth, key, ready := catalogOpenAIAuth(order, 0, entry)
		if !ready || auth == nil || auth.ID != "first" || key != "key-one" {
			t.Fatalf("selected auth=%+v key=%q", auth, key)
		}
	}
}

func TestFetchModelChannelCatalogReturnsNonJSONBodyAsString(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "demo", BaseURL: "https://example.com/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "key"}}}}}
	auth := &coreauth.Auth{ID: "openai", Provider: util.OpenAICompatibleProviderKey("demo"), Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "base_url": "https://example.com/v1", "api_key": "key"}}
	h, _ := newModelChannelTestHandler(t, "{}\n", cfg, auth)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	oldClient := modelCatalogHTTPClient
	modelCatalogHTTPClient = func(http.RoundTripper) *http.Client {
		return &http.Client{Transport: &recordingCatalogTransport{status: http.StatusBadGateway, body: []byte("plain upstream failure")}, Timeout: time.Second}
	}
	defer func() { modelCatalogHTTPClient = oldClient }()
	body := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","profile":"openai_models"}`
	response := performModelChannelRequest(h.FetchModelChannelCatalog, http.MethodPost, body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"body":"plain upstream failure"`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDisabledOpenAIChannelFailsClosed(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "demo", BaseURL: "https://example.com/v1", Disabled: true, APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "key"}}, Models: []config.OpenAICompatibilityModel{{Name: "one"}}}}}
	h, _ := newModelChannelTestHandler(t, "{}\n", cfg)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	membership := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"desired_model_names":["one"]}`
	if response := performModelChannelRequest(h.ReconcileModelChannelMembership, http.MethodPost, membership); response.Code != http.StatusConflict {
		t.Fatalf("membership status=%d body=%s", response.Code, response.Body.String())
	}
	metadata := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"operations":[{"model":"one","fields":{"max-output-tokens":{"mode":"replace","value":30}}}]}`
	if response := performModelChannelRequest(h.PatchModelChannelMetadata, http.MethodPatch, metadata); response.Code != http.StatusConflict {
		t.Fatalf("metadata status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestFetchModelChannelCatalogRedirectBodyCapAndTimeout(t *testing.T) {
	oldClient := modelCatalogHTTPClient
	defer func() { modelCatalogHTTPClient = oldClient }()
	client := modelCatalogHTTPClient(http.DefaultTransport)
	if err := client.CheckRedirect(httptest.NewRequest(http.MethodGet, "https://other.example", nil), nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error=%v", err)
	}

	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "demo", BaseURL: "https://example.com/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "key"}}}}}
	auth := &coreauth.Auth{ID: "openai", Provider: util.OpenAICompatibleProviderKey("demo"), Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "base_url": "https://example.com/v1", "api_key": "key"}}
	h, _ := newModelChannelTestHandler(t, "{}\n", cfg, auth)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	request := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","profile":"openai_models"}`

	modelCatalogHTTPClient = func(http.RoundTripper) *http.Client {
		return &http.Client{Transport: &recordingCatalogTransport{status: 200, body: bytesOfSize(modelCatalogMaxBody + 1)}, Timeout: time.Second}
	}
	if response := performModelChannelRequest(h.FetchModelChannelCatalog, http.MethodPost, request); response.Code != http.StatusBadGateway {
		t.Fatalf("body cap status=%d body=%s", response.Code, response.Body.String())
	}
	modelCatalogHTTPClient = func(http.RoundTripper) *http.Client {
		return &http.Client{Transport: &recordingCatalogTransport{delay: 50 * time.Millisecond}, Timeout: 5 * time.Millisecond}
	}
	if response := performModelChannelRequest(h.FetchModelChannelCatalog, http.MethodPost, request); response.Code != http.StatusBadGateway {
		t.Fatalf("timeout status=%d body=%s", response.Code, response.Body.String())
	}
}

func bytesOfSize(size int) []byte { return []byte(strings.Repeat("x", size)) }

func TestReconcileMembershipPreservesMetadataAndYAML(t *testing.T) {
	yamlConfig := `# root comment
openai-compatibility:
  - name: demo
    base-url: https://example.com/v1
    unknown-channel: keep
    models:
      - name: keep
        alias: custom
        max-output-tokens: 77
        unknown-model: preserve
      - name: remove
        alias: remove
claude-api-key:
  - base-url: https://claude.example
    models:
      - name: untouched
        alias: untouched
`
	cfg, err := config.ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatal(err)
	}
	h, path := newModelChannelTestHandler(t, yamlConfig, cfg)
	before, _ := os.Stat(path)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	body := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["keep","remove"],"desired_model_names":["keep","new"],"keep_existing_aliases":true}`
	response := performModelChannelRequest(h.ReconcileModelChannelMembership, http.MethodPost, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("config inode changed")
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	for _, want := range []string{"# root comment", "unknown-channel: keep", "unknown-model: preserve", "name: untouched", "name: new", "alias: custom", "max-output-tokens: 77"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "name: remove") {
		t.Fatalf("removed model remained:\n%s", text)
	}
	if cfg.OpenAICompatibility[0].Models[0].Alias != "custom" || cfg.OpenAICompatibility[0].Models[1].Name != "new" {
		t.Fatalf("memory=%+v", cfg.OpenAICompatibility[0].Models)
	}
}

func TestPatchMetadataModesAndAllOrNothing(t *testing.T) {
	yamlConfig := `openai-compatibility:
  - name: demo
    base-url: https://example.com/v1
    models:
      - name: one
        alias: one
        max-context-length: 10
`
	cfg, err := config.ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatal(err)
	}
	h, path := newModelChannelTestHandler(t, yamlConfig, cfg)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	body := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"operations":[{"model":"one","fields":{"max-context-length":{"mode":"if-empty","value":20},"max-output-tokens":{"mode":"replace","value":30},"input-modalities":{"mode":"replace","value":["TEXT","image"]}}}]}`
	response := performModelChannelRequest(h.PatchModelChannelMetadata, http.MethodPatch, body)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	model := cfg.OpenAICompatibility[0].Models[0]
	if model.MaxContextLength != 10 || model.MaxOutputTokens != 30 || strings.Join(model.InputModalities, ",") != "text,image" {
		t.Fatalf("model=%+v", model)
	}
	before, _ := os.ReadFile(path)
	nullPatch := strings.Replace(body, `"value":30`, `"value":null`, 1)
	if response = performModelChannelRequest(h.PatchModelChannelMetadata, http.MethodPatch, nullPatch); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("null status=%d body=%s", response.Code, response.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("rejected patch changed file")
	}
}

func TestPatchMetadataRejectsAnyDuplicateCurrentModel(t *testing.T) {
	yamlConfig := `openai-compatibility:
  - name: demo
    base-url: https://example.com/v1
    models:
      - name: one
        alias: one
        max-context-length: 10
      - name: duplicate
        alias: one
      - name: duplicate
        alias: two
`
	cfg, err := config.ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatal(err)
	}
	h, path := newModelChannelTestHandler(t, yamlConfig, cfg)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	body := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one","duplicate","duplicate"],"operations":[{"model":"one","fields":{"max-context-length":{"mode":"if-empty","value":20},"max-output-tokens":{"mode":"replace","value":30},"input-modalities":{"mode":"replace","value":["TEXT","image"]}}}]}`
	response := performModelChannelRequest(h.PatchModelChannelMetadata, http.MethodPatch, body)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != yamlConfig || cfg.OpenAICompatibility[0].Models[1].Alias != "one" || cfg.OpenAICompatibility[0].Models[2].Alias != "two" {
		t.Fatalf("duplicate entries changed: %s %+v", raw, cfg.OpenAICompatibility[0].Models)
	}
}

func TestPatchMetadataWriteFailureRollsBack(t *testing.T) {
	yamlConfig := `openai-compatibility:
  - name: demo
    base-url: https://example.com/v1
    models:
      - name: one
        alias: one
`
	cfg, err := config.ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatal(err)
	}
	h, path := newModelChannelTestHandler(t, yamlConfig, cfg)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	body := `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"operations":[{"model":"one","fields":{"max-output-tokens":{"mode":"replace","value":30}}}]}`
	oldWrite := modelChannelWriteInPlace
	calls := 0
	modelChannelWriteInPlace = func(path string, data []byte) error {
		calls++
		if calls == 1 {
			_ = os.WriteFile(path, []byte("partial"), 0o600)
			return errors.New("boom")
		}
		return writeModelChannelFileInPlace(path, data)
	}
	defer func() { modelChannelWriteInPlace = oldWrite }()
	response := performModelChannelRequest(h.PatchModelChannelMetadata, http.MethodPatch, body)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != yamlConfig {
		t.Fatalf("rollback=%q", raw)
	}
	backup, err := os.ReadFile(path + ".model-channel.bak")
	if err != nil || string(backup) != yamlConfig {
		t.Fatalf("recovery backup=%q err=%v", backup, err)
	}
	if cfg.OpenAICompatibility[0].Models[0].MaxOutputTokens != 0 {
		t.Fatal("in-memory state published after persistence failure")
	}
}

func TestModelChannelRevisionIsOpaqueKeyedAndSensitive(t *testing.T) {
	oldKey := modelChannelRevisionKey
	modelChannelRevisionKey = []byte("0123456789abcdef0123456789abcdef")
	defer func() { modelChannelRevisionKey = oldKey }()
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	entry := config.OpenAICompatibility{Name: "demo", BaseURL: selector.BaseURL, APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "secret"}}, Models: []config.OpenAICompatibilityModel{{Name: "one"}}}
	revision := openAIChannelRevision(&entry, selector)
	payload := struct {
		Kind     string               `json:"kind"`
		Selector modelChannelSelector `json:"selector"`
		Disabled bool                 `json:"disabled"`
		State    any                  `json:"state"`
		Models   any                  `json:"models"`
	}{
		Kind: modelChannelKindOpenAI, Selector: selector,
		State: struct {
			BaseURL       string                             `json:"base_url"`
			Prefix        string                             `json:"prefix"`
			GlobalProxy   string                             `json:"global_proxy"`
			Headers       map[string]string                  `json:"headers,omitempty"`
			APIKeyEntries []config.OpenAICompatibilityAPIKey `json:"api_key_entries,omitempty"`
		}{BaseURL: entry.BaseURL, APIKeyEntries: entry.APIKeyEntries}, Models: entry.Models,
	}
	raw, _ := json.Marshal(payload)
	plain := sha256.Sum256(raw)
	if revision == hex.EncodeToString(plain[:]) || strings.Contains(revision, "secret") {
		t.Fatalf("revision is reproducible or leaks secret: %s", revision)
	}
	if revision != openAIChannelRevision(&entry, selector) {
		t.Fatal("revision is not deterministic within process")
	}
	entry.APIKeyEntries[0].APIKey = "changed"
	if revision == openAIChannelRevision(&entry, selector) {
		t.Fatal("sensitive state did not change revision")
	}
}

func TestDiskSelectorDriftRejectsBeforeWrite(t *testing.T) {
	yamlConfig := `openai-compatibility:
  - name: one
    base-url: https://one.example/v1
    models:
      - name: one-model
        alias: one-model
  - name: two
    base-url: https://two.example/v1
    models:
      - name: two-model
        alias: two-model
`
	cfg, err := config.ParseConfigBytes([]byte(yamlConfig))
	if err != nil {
		t.Fatal(err)
	}
	h, path := newModelChannelTestHandler(t, yamlConfig, cfg)
	selector := modelChannelSelector{Name: "one", BaseURL: "https://one.example/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	reordered := strings.Replace(yamlConfig, "  - name: one\n", "  - name: changed\n", 1)
	if err := os.WriteFile(path, []byte(reordered), 0o600); err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"openai-compatibility","selector":{"name":"one","base_url":"https://one.example/v1"},"expected_revision":"` + revision + `","expected_model_names":["one-model"],"desired_model_names":["one-model","new"]}`
	response := performModelChannelRequest(h.ReconcileModelChannelMembership, http.MethodPost, body)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != reordered {
		t.Fatal("disk drift request changed config")
	}
}

func TestPatchThinkingPreservesNonLevelFields(t *testing.T) {
	current := &registry.ThinkingSupport{Min: 1, Max: 10, ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"low"}}
	levels := []string{"high"}
	patchThinking(&current, &metadataFieldPatch[[]string]{Mode: "replace", Value: &levels}, levels)
	if current.Min != 1 || current.Max != 10 || !current.ZeroAllowed || !current.DynamicAllowed || strings.Join(current.Levels, ",") != "high" {
		t.Fatalf("thinking=%+v", current)
	}
}

func TestCatalogAuthRejectsStaleStateAndMultiKeyReady(t *testing.T) {
	entry := &config.OpenAICompatibility{Name: "demo", BaseURL: "https://example.com/v1", APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "one"}, {APIKey: "two", ProxyURL: "http://proxy.example"}}}
	auths := []*coreauth.Auth{
		{ID: "one", Provider: util.OpenAICompatibleProviderKey("demo"), Status: coreauth.StatusActive, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "base_url": entry.BaseURL, "api_key": "one"}},
		{ID: "two", Provider: util.OpenAICompatibleProviderKey("demo"), Status: coreauth.StatusActive, ProxyURL: "http://proxy.example", Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "compat_name": "demo", "base_url": entry.BaseURL, "api_key": "two"}},
	}
	_, key, ready := catalogOpenAIAuth(auths, 0, entry)
	if !ready || key != "one" {
		t.Fatalf("ready=%v key=%q", ready, key)
	}
	auths[0].Attributes["base_url"] = "https://stale.example/v1"
	_, key, ready = catalogOpenAIAuth(auths, 0, entry)
	if !ready || key != "two" {
		t.Fatalf("stale first credential should fall through: ready=%v key=%q", ready, key)
	}
	auths[1].ProxyURL = "http://stale-proxy.example"
	if _, _, ready = catalogOpenAIAuth(auths, 0, entry); ready {
		t.Fatal("stale base/proxy credential accepted")
	}

	claude := &config.ClaudeKey{APIKey: "claude", BaseURL: "https://claude.example/v1/messages", Prefix: "p", ProxyURL: "http://proxy.example"}
	claudeAuth := &coreauth.Auth{ID: "claude", Provider: "claude", Prefix: "p", Status: coreauth.StatusActive, ProxyURL: claude.ProxyURL, Attributes: map[string]string{coreauth.AttributeConfigIndex: "0", "base_url": claude.BaseURL, "api_key": claude.APIKey}}
	if _, _, ready := catalogClaudeAuth([]*coreauth.Auth{claudeAuth}, 0, claude); !ready {
		t.Fatal("current Claude auth not ready")
	}
	claudeAuth.Attributes["base_url"] = "https://stale.example/v1/messages"
	if _, _, ready := catalogClaudeAuth([]*coreauth.Auth{claudeAuth}, 0, claude); ready {
		t.Fatal("stale Claude auth accepted")
	}
}

func TestModelChannelHandlersRejectTrailingJSON(t *testing.T) {
	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{Name: "demo", BaseURL: "https://example.com/v1", Models: []config.OpenAICompatibilityModel{{Name: "one"}}}}}
	h, _ := newModelChannelTestHandler(t, "openai-compatibility:\n  - name: demo\n    base-url: https://example.com/v1\n    models:\n      - name: one\n", cfg)
	selector := modelChannelSelector{Name: "demo", BaseURL: "https://example.com/v1"}
	revision := openAIChannelRevision(&cfg.OpenAICompatibility[0], selector)
	requests := []struct {
		handler gin.HandlerFunc
		method  string
		body    string
	}{
		{h.FetchModelChannelCatalog, http.MethodPost, `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","profile":"openai_models"}{}`},
		{h.ReconcileModelChannelMembership, http.MethodPost, `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"desired_model_names":["one"]}{}`},
		{h.PatchModelChannelMetadata, http.MethodPatch, `{"kind":"openai-compatibility","selector":{"name":"demo","base_url":"https://example.com/v1"},"expected_revision":"` + revision + `","expected_model_names":["one"],"operations":[{"model":"one","fields":{"max-output-tokens":{"mode":"replace","value":1}}}]}{}`},
	}
	for _, request := range requests {
		if response := performModelChannelRequest(request.handler, request.method, request.body); response.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
}
