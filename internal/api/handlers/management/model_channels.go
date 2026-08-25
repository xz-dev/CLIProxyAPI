package management

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelconfig"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	modelChannelKindOpenAI  = "openai-compatibility"
	modelChannelKindClaude  = "claude"
	modelCatalogTimeout     = 30 * time.Second
	modelCatalogMaxBody     = 8 << 20
	anthropicDefaultBaseURL = "https://api.anthropic.com"
)

var (
	modelChannelRevisionKey  = mustModelChannelRevisionKey()
	modelChannelWriteInPlace = writeModelChannelFileInPlace
	modelCatalogHTTPClient   = func(transport http.RoundTripper) *http.Client {
		return &http.Client{
			Timeout:   modelCatalogTimeout,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
)

type modelChannelSelector struct {
	Name        string `json:"name,omitempty"`
	ConfigIndex *int   `json:"config_index,omitempty"`
	BaseURL     string `json:"base_url"`
	Prefix      string `json:"prefix,omitempty"`
}

type modelChannelModel struct {
	Name             string                    `json:"name"`
	Alias            string                    `json:"alias,omitempty"`
	DisplayName      string                    `json:"display_name,omitempty"`
	ForceMapping     bool                      `json:"force_mapping,omitempty"`
	Image            bool                      `json:"image,omitempty"`
	IsCompat         bool                      `json:"is_compat,omitempty"`
	Thinking         *registry.ThinkingSupport `json:"thinking,omitempty"`
	MaxContextLength int                       `json:"max_context_length,omitempty"`
	MaxInputTokens   int                       `json:"max_input_tokens,omitempty"`
	MaxOutputTokens  int                       `json:"max_output_tokens,omitempty"`
	InputModalities  []string                  `json:"input_modalities,omitempty"`
	OutputModalities []string                  `json:"output_modalities,omitempty"`
}

type modelChannelDescriptor struct {
	Kind        string               `json:"kind"`
	Selector    modelChannelSelector `json:"selector"`
	Disabled    bool                 `json:"disabled"`
	Ready       bool                 `json:"ready"`
	BaseURL     string               `json:"base_url"`
	Prefix      string               `json:"prefix,omitempty"`
	ConfigIndex *int                 `json:"config_index,omitempty"`
	Revision    string               `json:"revision"`
	Models      []modelChannelModel  `json:"models"`
}

type modelChannelError struct {
	Status int
	Msg    string
}

func (e *modelChannelError) Error() string { return e.Msg }

func channelError(status int, format string, args ...any) error {
	return &modelChannelError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

func writeModelChannelError(c *gin.Context, err error) {
	var channelErr *modelChannelError
	if errors.As(err, &channelErr) {
		c.JSON(channelErr.Status, gin.H{"error": channelErr.Msg})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal model channel error"})
}

// canonicalizeModelChannelURL normalizes selector URLs without discarding path or query identity.
func canonicalizeModelChannelURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base_url must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("base_url must not contain userinfo or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("base_url scheme must be http or https")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	parsed.Host = host
	if parsed.Path == "/" {
		parsed.Path = ""
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	return parsed.String(), nil
}

func effectiveClaudeBaseURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return anthropicDefaultBaseURL
	}
	return raw
}

func normalizeChannelPrefix(raw string) string {
	return strings.Trim(strings.TrimSpace(raw), "/")
}

func mustModelChannelRevisionKey() []byte {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("initialize model channel revision key: %v", err))
	}
	return key
}

func modelChannelRevision(kind string, selector modelChannelSelector, disabled bool, state any, models any) string {
	payload := struct {
		Kind     string               `json:"kind"`
		Selector modelChannelSelector `json:"selector"`
		Disabled bool                 `json:"disabled"`
		State    any                  `json:"state"`
		Models   any                  `json:"models"`
	}{Kind: kind, Selector: selector, Disabled: disabled, State: state, Models: models}
	data, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, modelChannelRevisionKey)
	_, _ = mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil))
}

func openAIChannelRevision(entry *config.OpenAICompatibility, selector modelChannelSelector, globalProxy ...string) string {
	if entry == nil {
		return ""
	}
	proxy := ""
	if len(globalProxy) > 0 {
		proxy = globalProxy[0]
	}
	state := struct {
		BaseURL       string                             `json:"base_url"`
		Prefix        string                             `json:"prefix"`
		GlobalProxy   string                             `json:"global_proxy"`
		Headers       map[string]string                  `json:"headers,omitempty"`
		APIKeyEntries []config.OpenAICompatibilityAPIKey `json:"api_key_entries,omitempty"`
	}{BaseURL: entry.BaseURL, Prefix: entry.Prefix, GlobalProxy: proxy, Headers: entry.Headers, APIKeyEntries: entry.APIKeyEntries}
	return modelChannelRevision(modelChannelKindOpenAI, selector, entry.Disabled, state, entry.Models)
}

func claudeChannelRevision(entry *config.ClaudeKey, selector modelChannelSelector, globalProxy ...string) string {
	if entry == nil {
		return ""
	}
	proxy := ""
	if len(globalProxy) > 0 {
		proxy = globalProxy[0]
	}
	state := struct {
		APIKey      string            `json:"api_key"`
		BaseURL     string            `json:"base_url"`
		ProxyURL    string            `json:"proxy_url"`
		GlobalProxy string            `json:"global_proxy"`
		Prefix      string            `json:"prefix"`
		Headers     map[string]string `json:"headers,omitempty"`
	}{APIKey: entry.APIKey, BaseURL: entry.BaseURL, ProxyURL: entry.ProxyURL, GlobalProxy: proxy, Prefix: entry.Prefix, Headers: entry.Headers}
	return modelChannelRevision(modelChannelKindClaude, selector, false, state, entry.Models)
}

func liveModelChannelAuths(auths []*coreauth.Auth, kind string, configIndex int) []*coreauth.Auth {
	index := strconv.Itoa(configIndex)
	matches := make([]*coreauth.Auth, 0)
	for _, candidate := range auths {
		if candidate == nil || candidate.Attributes == nil || strings.TrimSpace(candidate.Attributes[coreauth.AttributeConfigIndex]) != index {
			continue
		}
		matchesKind := kind == modelChannelKindClaude && strings.EqualFold(candidate.Provider, "claude")
		if kind == modelChannelKindOpenAI {
			matchesKind = strings.TrimSpace(candidate.Attributes["compat_name"]) != ""
		}
		if !matchesKind || candidate.Disabled || candidate.Unavailable || candidate.Status != coreauth.StatusActive || strings.TrimSpace(candidate.Attributes["api_key"]) == "" {
			continue
		}
		matches = append(matches, candidate)
	}
	return matches
}

func normalizedAuthBaseURL(auth *coreauth.Auth, fallback string, allowMissing bool) (string, bool) {
	if auth == nil || auth.Attributes == nil {
		return "", false
	}
	base := strings.TrimSpace(auth.Attributes["base_url"])
	if base == "" {
		if !allowMissing {
			return "", false
		}
		base = fallback
	}
	normalized, err := canonicalizeModelChannelURL(base)
	return normalized, err == nil
}

func openAIAuthMatchesChannel(auth *coreauth.Auth, index int, entry *config.OpenAICompatibility, key config.OpenAICompatibilityAPIKey) bool {
	if auth == nil || entry == nil || auth.Attributes == nil || strings.TrimSpace(auth.Attributes[coreauth.AttributeConfigIndex]) != strconv.Itoa(index) {
		return false
	}
	if auth.Disabled || auth.Unavailable || auth.Status != coreauth.StatusActive || auth.Provider != util.OpenAICompatibleProviderKey(entry.Name) {
		return false
	}
	if strings.TrimSpace(auth.Attributes["compat_name"]) != strings.TrimSpace(entry.Name) || strings.TrimSpace(auth.Attributes["api_key"]) != strings.TrimSpace(key.APIKey) {
		return false
	}
	authBase, ok := normalizedAuthBaseURL(auth, entry.BaseURL, false)
	channelBase, err := canonicalizeModelChannelURL(entry.BaseURL)
	return ok && err == nil && authBase == channelBase && strings.TrimSpace(auth.ProxyURL) == strings.TrimSpace(key.ProxyURL)
}

func claudeAuthMatchesChannel(auth *coreauth.Auth, index int, entry *config.ClaudeKey) bool {
	if auth == nil || entry == nil || auth.Attributes == nil || strings.TrimSpace(auth.Attributes[coreauth.AttributeConfigIndex]) != strconv.Itoa(index) {
		return false
	}
	if auth.Disabled || auth.Unavailable || auth.Status != coreauth.StatusActive || !strings.EqualFold(auth.Provider, "claude") || strings.TrimSpace(auth.Attributes["api_key"]) != strings.TrimSpace(entry.APIKey) {
		return false
	}
	authBase, ok := normalizedAuthBaseURL(auth, effectiveClaudeBaseURL(entry.BaseURL), strings.TrimSpace(entry.BaseURL) == "")
	channelBase, err := canonicalizeModelChannelURL(effectiveClaudeBaseURL(entry.BaseURL))
	return ok && err == nil && authBase == channelBase && normalizeChannelPrefix(auth.Prefix) == normalizeChannelPrefix(entry.Prefix) && strings.TrimSpace(auth.ProxyURL) == strings.TrimSpace(entry.ProxyURL)
}

func openAIModelDescriptor(model config.OpenAICompatibilityModel) modelChannelModel {
	return modelChannelModel{
		Name: model.Name, Alias: model.Alias, DisplayName: model.DisplayName,
		ForceMapping: model.ForceMapping, Image: model.Image, IsCompat: model.IsCompat,
		Thinking:         modelconfig.NormalizeThinkingSupport(model.Thinking),
		MaxContextLength: model.MaxContextLength, MaxInputTokens: model.MaxInputTokens, MaxOutputTokens: model.MaxOutputTokens,
		InputModalities: append([]string(nil), model.InputModalities...), OutputModalities: append([]string(nil), model.OutputModalities...),
	}
}

func claudeModelDescriptor(model config.ClaudeModel) modelChannelModel {
	return modelChannelModel{
		Name: model.Name, Alias: model.Alias, DisplayName: model.DisplayName,
		ForceMapping: model.ForceMapping, IsCompat: model.IsCompat,
		Thinking:         modelconfig.NormalizeThinkingSupport(model.Thinking),
		MaxContextLength: model.MaxContextLength, MaxInputTokens: model.MaxInputTokens, MaxOutputTokens: model.MaxOutputTokens,
		InputModalities: append([]string(nil), model.InputModalities...), OutputModalities: append([]string(nil), model.OutputModalities...),
	}
}

// GetModelChannels returns closed, sanitized model-channel descriptors.
func (h *Handler) GetModelChannels(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusOK, gin.H{"channels": []modelChannelDescriptor{}})
		return
	}
	auths := []*coreauth.Auth(nil)
	if h.authManager != nil {
		auths = h.authManager.List()
	}
	channels := make([]modelChannelDescriptor, 0, len(h.cfg.OpenAICompatibility)+len(h.cfg.ClaudeKey))
	for i := range h.cfg.OpenAICompatibility {
		entry := &h.cfg.OpenAICompatibility[i]
		baseURL, err := canonicalizeModelChannelURL(entry.BaseURL)
		if err != nil {
			continue
		}
		selector := modelChannelSelector{Name: strings.TrimSpace(entry.Name), BaseURL: baseURL}
		_, _, ready := catalogOpenAIAuth(auths, i, entry)
		models := make([]modelChannelModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			models = append(models, openAIModelDescriptor(model))
		}
		channels = append(channels, modelChannelDescriptor{Kind: modelChannelKindOpenAI, Selector: selector, Disabled: entry.Disabled, Ready: ready, BaseURL: baseURL, Prefix: normalizeChannelPrefix(entry.Prefix), Revision: openAIChannelRevision(entry, selector, h.cfg.ProxyURL), Models: models})
	}
	for i := range h.cfg.ClaudeKey {
		entry := &h.cfg.ClaudeKey[i]
		baseURL, err := canonicalizeModelChannelURL(effectiveClaudeBaseURL(entry.BaseURL))
		if err != nil {
			continue
		}
		configIndex := i
		prefix := normalizeChannelPrefix(entry.Prefix)
		selector := modelChannelSelector{ConfigIndex: &configIndex, BaseURL: baseURL, Prefix: prefix}
		_, _, ready := catalogClaudeAuth(auths, i, entry)
		models := make([]modelChannelModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			models = append(models, claudeModelDescriptor(model))
		}
		channels = append(channels, modelChannelDescriptor{Kind: modelChannelKindClaude, Selector: selector, Ready: ready, BaseURL: baseURL, Prefix: prefix, ConfigIndex: &configIndex, Revision: claudeChannelRevision(entry, selector, h.cfg.ProxyURL), Models: models})
	}
	c.JSON(http.StatusOK, gin.H{"channels": channels})
}

func normalizeSelector(kind string, raw modelChannelSelector) (modelChannelSelector, error) {
	baseURL, err := canonicalizeModelChannelURL(raw.BaseURL)
	if err != nil {
		return modelChannelSelector{}, channelError(http.StatusBadRequest, "invalid selector base_url: %v", err)
	}
	switch kind {
	case modelChannelKindOpenAI:
		name := strings.TrimSpace(raw.Name)
		if name == "" || raw.ConfigIndex != nil || normalizeChannelPrefix(raw.Prefix) != "" {
			return modelChannelSelector{}, channelError(http.StatusBadRequest, "openai-compatibility selector requires name and base_url only")
		}
		return modelChannelSelector{Name: name, BaseURL: baseURL}, nil
	case modelChannelKindClaude:
		if raw.ConfigIndex == nil || *raw.ConfigIndex < 0 || strings.TrimSpace(raw.Name) != "" {
			return modelChannelSelector{}, channelError(http.StatusBadRequest, "claude selector requires config_index, base_url, and prefix")
		}
		index := *raw.ConfigIndex
		return modelChannelSelector{ConfigIndex: &index, BaseURL: baseURL, Prefix: normalizeChannelPrefix(raw.Prefix)}, nil
	default:
		return modelChannelSelector{}, channelError(http.StatusBadRequest, "unsupported channel kind %q", kind)
	}
}

func (h *Handler) resolveOpenAIChannelLocked(raw modelChannelSelector) (int, *config.OpenAICompatibility, modelChannelSelector, error) {
	selector, err := normalizeSelector(modelChannelKindOpenAI, raw)
	if err != nil {
		return -1, nil, modelChannelSelector{}, err
	}
	match := -1
	for i := range h.cfg.OpenAICompatibility {
		entry := &h.cfg.OpenAICompatibility[i]
		baseURL, errBase := canonicalizeModelChannelURL(entry.BaseURL)
		if errBase == nil && strings.TrimSpace(entry.Name) == selector.Name && baseURL == selector.BaseURL {
			if match >= 0 {
				return -1, nil, selector, channelError(http.StatusConflict, "channel selector is ambiguous")
			}
			match = i
		}
	}
	if match < 0 {
		return -1, nil, selector, channelError(http.StatusNotFound, "channel selector not found")
	}
	return match, &h.cfg.OpenAICompatibility[match], selector, nil
}

func (h *Handler) resolveClaudeChannelLocked(raw modelChannelSelector) (int, *config.ClaudeKey, modelChannelSelector, error) {
	selector, err := normalizeSelector(modelChannelKindClaude, raw)
	if err != nil {
		return -1, nil, modelChannelSelector{}, err
	}
	index := *selector.ConfigIndex
	if index >= len(h.cfg.ClaudeKey) {
		return -1, nil, selector, channelError(http.StatusConflict, "claude config_index drifted")
	}
	entry := &h.cfg.ClaudeKey[index]
	baseURL, errBase := canonicalizeModelChannelURL(effectiveClaudeBaseURL(entry.BaseURL))
	if errBase != nil || baseURL != selector.BaseURL || normalizeChannelPrefix(entry.Prefix) != selector.Prefix {
		return -1, nil, selector, channelError(http.StatusConflict, "claude selector drifted")
	}
	return index, entry, selector, nil
}

func modelNamesOpenAI(models []config.OpenAICompatibilityModel) []string {
	out := make([]string, len(models))
	for i := range models {
		out[i] = models[i].Name
	}
	return out
}

func modelNamesClaude(models []config.ClaudeModel) []string {
	out := make([]string, len(models))
	for i := range models {
		out[i] = models[i].Name
	}
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

type modelCatalogQuery struct {
	ClientVersion string `json:"client_version,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	AfterID       string `json:"after_id,omitempty"`
	BeforeID      string `json:"before_id,omitempty"`
}

type modelCatalogRequest struct {
	Kind             string               `json:"kind"`
	Selector         modelChannelSelector `json:"selector"`
	ExpectedRevision string               `json:"expected_revision"`
	Profile          string               `json:"profile"`
	Query            *modelCatalogQuery   `json:"query,omitempty"`
}

func catalogTarget(baseURL, profile string) (*url.URL, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch profile {
	case "openai_models":
		if !strings.HasSuffix(path, "/models") {
			path += "/models"
		}
	case "claude_models":
		path = strings.TrimSuffix(path, "/v1/messages")
		if !strings.HasSuffix(path, "/v1/models") {
			path += "/v1/models"
		}
	default:
		return nil, fmt.Errorf("unsupported catalog profile")
	}
	parsed.Path = path
	return parsed, nil
}

func copyConfiguredHeaders(dst http.Header, configured map[string]string) {
	for key, value := range configured {
		value = strings.TrimSpace(value)
		if strings.TrimSpace(key) != "" && value != "" && !strings.HasPrefix(value, "$") {
			dst.Set(key, value)
		}
	}
}

func catalogOpenAIAuth(auths []*coreauth.Auth, index int, entry *config.OpenAICompatibility) (*coreauth.Auth, string, bool) {
	if entry == nil {
		return nil, "", false
	}
	matches := liveModelChannelAuths(auths, modelChannelKindOpenAI, index)
	sort.Slice(matches, func(i, j int) bool { return matches[i].ID < matches[j].ID })
	for _, configured := range entry.APIKeyEntries {
		key := strings.TrimSpace(configured.APIKey)
		if key == "" {
			continue
		}
		for _, auth := range matches {
			if openAIAuthMatchesChannel(auth, index, entry, configured) {
				return auth, key, true
			}
		}
	}
	return nil, "", false
}

func catalogClaudeAuth(auths []*coreauth.Auth, index int, entry *config.ClaudeKey) (*coreauth.Auth, string, bool) {
	for _, auth := range liveModelChannelAuths(auths, modelChannelKindClaude, index) {
		if claudeAuthMatchesChannel(auth, index, entry) {
			return auth, strings.TrimSpace(entry.APIKey), true
		}
	}
	return nil, "", false
}

// FetchModelChannelCatalog performs a credential-bound catalog GET using an allowlisted profile.
func modelCatalogTransport(channelProxy, globalProxy string) (http.RoundTripper, error) {
	proxyURL := strings.TrimSpace(channelProxy)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(globalProxy)
	}
	if proxyURL == "" {
		return directAPICallTransport(), nil
	}
	transport := buildProxyTransport(proxyURL)
	if transport == nil {
		return nil, fmt.Errorf("configured channel proxy is invalid")
	}
	return transport, nil
}

func decodeStrictJSON(body io.Reader, target any) error {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func (h *Handler) FetchModelChannelCatalog(c *gin.Context) {
	var body modelCatalogRequest
	if err := decodeStrictJSON(c.Request.Body, &body); err != nil || strings.TrimSpace(body.ExpectedRevision) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body or missing expected_revision"})
		return
	}

	h.mu.Lock()
	if h.cfg == nil {
		h.mu.Unlock()
		c.JSON(http.StatusNotFound, gin.H{"error": "channel selector not found"})
		return
	}
	auths := []*coreauth.Auth(nil)
	if h.authManager != nil {
		auths = h.authManager.List()
	}
	var (
		index           int
		selector        modelChannelSelector
		baseURL, apiKey string
		headers         map[string]string
		proxyURL        string
		globalProxy     string
		auth            *coreauth.Auth
		revision        string
		ready           bool
	)
	switch body.Kind {
	case modelChannelKindOpenAI:
		var entry *config.OpenAICompatibility
		var err error
		index, entry, selector, err = h.resolveOpenAIChannelLocked(body.Selector)
		if err == nil {
			revision = openAIChannelRevision(entry, selector, h.cfg.ProxyURL)
			baseURL = selector.BaseURL
			headers = copyStringMap(entry.Headers)
			auth, apiKey, ready = catalogOpenAIAuth(auths, index, entry)
			if ready {
				for _, configured := range entry.APIKeyEntries {
					if strings.TrimSpace(configured.APIKey) == apiKey {
						proxyURL = strings.TrimSpace(configured.ProxyURL)
						break
					}
				}
			}
			if entry.Disabled {
				err = channelError(http.StatusConflict, "channel is disabled")
			}
		}
		if err != nil {
			h.mu.Unlock()
			writeModelChannelError(c, err)
			return
		}
	case modelChannelKindClaude:
		var entry *config.ClaudeKey
		var err error
		index, entry, selector, err = h.resolveClaudeChannelLocked(body.Selector)
		if err == nil {
			revision = claudeChannelRevision(entry, selector, h.cfg.ProxyURL)
			baseURL = selector.BaseURL
			headers = copyStringMap(entry.Headers)
			proxyURL = strings.TrimSpace(entry.ProxyURL)
			auth, apiKey, ready = catalogClaudeAuth(auths, index, entry)
		}
		if err != nil {
			h.mu.Unlock()
			writeModelChannelError(c, err)
			return
		}
	default:
		h.mu.Unlock()
		writeModelChannelError(c, channelError(http.StatusBadRequest, "unsupported channel kind %q", body.Kind))
		return
	}
	globalProxy = h.cfg.ProxyURL
	h.mu.Unlock()

	if body.ExpectedRevision != revision {
		c.JSON(http.StatusConflict, gin.H{"error": "channel revision drifted"})
		return
	}
	if !ready || auth == nil || apiKey == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "live channel credential is not ready or has drifted"})
		return
	}

	var target *url.URL
	var err error
	switch body.Profile {
	case "openai_models":
		if body.Kind != modelChannelKindOpenAI {
			writeModelChannelError(c, channelError(http.StatusUnprocessableEntity, "openai_models requires openai-compatibility channel"))
			return
		}
		target, err = catalogTarget(baseURL, body.Profile)
		if body.Query != nil {
			if body.Query.Limit != 0 || body.Query.AfterID != "" || body.Query.BeforeID != "" || (body.Query.ClientVersion != "" && body.Query.ClientVersion != "1.0.0") {
				writeModelChannelError(c, channelError(http.StatusBadRequest, "unsupported openai_models query"))
				return
			}
			if body.Query.ClientVersion != "" {
				query := target.Query()
				query.Set("client_version", "1.0.0")
				target.RawQuery = query.Encode()
			}
		}
	case "claude_models":
		if body.Kind != modelChannelKindClaude {
			writeModelChannelError(c, channelError(http.StatusUnprocessableEntity, "claude_models requires claude channel"))
			return
		}
		target, err = catalogTarget(baseURL, body.Profile)
		if body.Query != nil {
			if body.Query.ClientVersion != "" || body.Query.Limit < 0 || body.Query.Limit > 1000 {
				writeModelChannelError(c, channelError(http.StatusBadRequest, "unsupported claude_models query"))
				return
			}
			query := target.Query()
			if body.Query.Limit > 0 {
				query.Set("limit", strconv.Itoa(body.Query.Limit))
			}
			if body.Query.AfterID != "" {
				query.Set("after_id", body.Query.AfterID)
			}
			if body.Query.BeforeID != "" {
				query.Set("before_id", body.Query.BeforeID)
			}
			target.RawQuery = query.Encode()
		}
	default:
		writeModelChannelError(c, channelError(http.StatusUnprocessableEntity, "unsupported catalog profile %q", body.Profile))
		return
	}
	if err != nil {
		writeModelChannelError(c, channelError(http.StatusBadRequest, "invalid catalog URL"))
		return
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeModelChannelError(c, channelError(http.StatusBadRequest, "failed to build catalog request"))
		return
	}
	copyConfiguredHeaders(req.Header, headers)
	if body.Kind == modelChannelKindClaude {
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
		req.Header.Set("Anthropic-Version", "2023-06-01")
		if strings.EqualFold(target.Scheme, "https") && strings.EqualFold(target.Hostname(), "api.anthropic.com") {
			req.Header.Set("x-api-key", apiKey)
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	transport, err := modelCatalogTransport(proxyURL, globalProxy)
	if err != nil {
		writeModelChannelError(c, channelError(http.StatusBadRequest, "%v", err))
		return
	}
	resp, err := modelCatalogHTTPClient(transport).Do(req)
	if err != nil {
		log.WithError(err).Debug("management model channel catalog request failed")
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream catalog request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("model catalog response close failed")
		}
	}()
	limited := io.LimitReader(resp.Body, modelCatalogMaxBody+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to read upstream catalog response"})
		return
	}
	if len(responseBody) > modelCatalogMaxBody {
		c.JSON(http.StatusBadGateway, gin.H{"error": "upstream catalog response exceeds body limit"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status_code": resp.StatusCode, "body": string(responseBody)})
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

type modelMembershipRequest struct {
	Kind                string               `json:"kind"`
	Selector            modelChannelSelector `json:"selector"`
	ExpectedRevision    string               `json:"expected_revision"`
	ExpectedModelNames  []string             `json:"expected_model_names"`
	DesiredModelNames   []string             `json:"desired_model_names"`
	KeepExistingAliases bool                 `json:"keep_existing_aliases"`
}

func normalizeDesiredModelNames(raw []string) ([]string, error) {
	if raw == nil {
		return nil, channelError(http.StatusBadRequest, "desired_model_names is required")
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for i, value := range raw {
		name := strings.TrimSpace(value)
		if name == "" {
			return nil, channelError(http.StatusBadRequest, "desired_model_names[%d] is empty", i)
		}
		if _, exists := seen[name]; exists {
			return nil, channelError(http.StatusConflict, "desired_model_names contains duplicate %q", name)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// ReconcileModelChannelMembership atomically reconciles one OpenAI-compatible model set.
func rejectDuplicateModelNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, exists := seen[name]; exists {
			return channelError(http.StatusConflict, "current model name %q is ambiguous", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func (h *Handler) ReconcileModelChannelMembership(c *gin.Context) {
	var body modelMembershipRequest
	if err := decodeStrictJSON(c.Request.Body, &body); err != nil || strings.TrimSpace(body.ExpectedRevision) == "" || body.ExpectedModelNames == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body or missing precondition"})
		return
	}
	if body.Kind != modelChannelKindOpenAI {
		writeModelChannelError(c, channelError(http.StatusUnprocessableEntity, "membership reconciliation supports openai-compatibility only"))
		return
	}
	desired, err := normalizeDesiredModelNames(body.DesiredModelNames)
	if err != nil {
		writeModelChannelError(c, err)
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel selector not found"})
		return
	}
	index, entry, selector, err := h.resolveOpenAIChannelLocked(body.Selector)
	if err != nil {
		writeModelChannelError(c, err)
		return
	}
	if entry.Disabled {
		c.JSON(http.StatusConflict, gin.H{"error": "channel is disabled"})
		return
	}
	currentNames := modelNamesOpenAI(entry.Models)
	if err := rejectDuplicateModelNames(currentNames); err != nil {
		writeModelChannelError(c, err)
		return
	}
	if body.ExpectedRevision != openAIChannelRevision(entry, selector, h.cfg.ProxyURL) || !equalStrings(body.ExpectedModelNames, currentNames) {
		c.JSON(http.StatusConflict, gin.H{"error": "channel revision or model-set precondition drifted"})
		return
	}
	byName := make(map[string]config.OpenAICompatibilityModel, len(entry.Models))
	for _, model := range entry.Models {
		byName[model.Name] = model
	}
	updated := make([]config.OpenAICompatibilityModel, 0, len(desired))
	for _, name := range desired {
		if existing, ok := byName[name]; ok {
			if !body.KeepExistingAliases {
				existing.Alias = name
			}
			updated = append(updated, existing)
		} else {
			updated = append(updated, config.OpenAICompatibilityModel{Name: name, Alias: name})
		}
	}
	candidate := h.cfg.CloneForRuntime()
	candidate.OpenAICompatibility[index].Models = updated
	if err := h.persistModelChannelCandidateLocked(candidate, body.Kind, index, selector, body.ExpectedRevision, body.ExpectedModelNames); err != nil {
		writeModelChannelError(c, err)
		return
	}
	h.cfg.OpenAICompatibility[index].Models = updated
	snapshot := h.reloadSnapshotConfigLocked()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "revision": openAIChannelRevision(&h.cfg.OpenAICompatibility[index], selector, h.cfg.ProxyURL)})
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
}

type metadataFieldPatch[T any] struct {
	Mode  string `json:"mode"`
	Value *T     `json:"value"`
}

type modelMetadataFields struct {
	ThinkingLevels   *metadataFieldPatch[[]string] `json:"thinking.levels,omitempty"`
	MaxContext       *metadataFieldPatch[int]      `json:"max-context-length,omitempty"`
	MaxInput         *metadataFieldPatch[int]      `json:"max-input-tokens,omitempty"`
	MaxOutput        *metadataFieldPatch[int]      `json:"max-output-tokens,omitempty"`
	InputModalities  *metadataFieldPatch[[]string] `json:"input-modalities,omitempty"`
	OutputModalities *metadataFieldPatch[[]string] `json:"output-modalities,omitempty"`
}

type modelMetadataOperation struct {
	Model  string              `json:"model"`
	Fields modelMetadataFields `json:"fields"`
}

type modelMetadataRequest struct {
	Kind               string                   `json:"kind"`
	Selector           modelChannelSelector     `json:"selector"`
	ExpectedRevision   string                   `json:"expected_revision"`
	ExpectedModelNames []string                 `json:"expected_model_names"`
	Operations         []modelMetadataOperation `json:"operations"`
}

func validatePatchMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "replace" && mode != "if-empty" {
		return "", channelError(http.StatusUnprocessableEntity, "unsupported metadata mode %q", mode)
	}
	return mode, nil
}

func validateTokenPatch(name string, patch *metadataFieldPatch[int]) error {
	if patch == nil {
		return nil
	}
	if patch.Value == nil {
		return channelError(http.StatusUnprocessableEntity, "%s null/clear is unsupported", name)
	}
	if *patch.Value <= 0 {
		return channelError(http.StatusBadRequest, "%s must be positive", name)
	}
	_, err := validatePatchMode(patch.Mode)
	return err
}

func validateStringSlicePatch(name string, patch *metadataFieldPatch[[]string], modalities bool) ([]string, error) {
	if patch == nil {
		return nil, nil
	}
	if patch.Value == nil {
		return nil, channelError(http.StatusUnprocessableEntity, "%s null/clear is unsupported", name)
	}
	if _, err := validatePatchMode(patch.Mode); err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(*patch.Value))
	out := make([]string, 0, len(*patch.Value))
	for _, raw := range *patch.Value {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			return nil, channelError(http.StatusBadRequest, "%s contains empty value", name)
		}
		if modalities && value != "text" && value != "image" {
			return nil, channelError(http.StatusUnprocessableEntity, "%s contains unsupported modality %q", name, raw)
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, channelError(http.StatusUnprocessableEntity, "%s clear is unsupported", name)
	}
	return out, nil
}

func patchInt(current *int, patch *metadataFieldPatch[int]) {
	if patch == nil {
		return
	}
	mode, _ := validatePatchMode(patch.Mode)
	if mode == "replace" || *current == 0 {
		*current = *patch.Value
	}
}

func patchSlice(current *[]string, patch *metadataFieldPatch[[]string], normalized []string) {
	if patch == nil {
		return
	}
	mode, _ := validatePatchMode(patch.Mode)
	if mode == "replace" || len(*current) == 0 {
		*current = append([]string(nil), normalized...)
	}
}

func patchThinking(current **registry.ThinkingSupport, patch *metadataFieldPatch[[]string], normalized []string) {
	if patch == nil {
		return
	}
	mode, _ := validatePatchMode(patch.Mode)
	if mode != "replace" && *current != nil && len((*current).Levels) > 0 {
		return
	}
	updated := &registry.ThinkingSupport{}
	if *current != nil {
		*updated = **current
	}
	updated.Levels = append([]string(nil), normalized...)
	*current = updated
}

// PatchModelChannelMetadata atomically patches allowlisted rich fields on existing models.
func (h *Handler) PatchModelChannelMetadata(c *gin.Context) {
	var body modelMetadataRequest
	if err := decodeStrictJSON(c.Request.Body, &body); err != nil || strings.TrimSpace(body.ExpectedRevision) == "" || body.ExpectedModelNames == nil || len(body.Operations) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body or missing precondition/operations"})
		return
	}

	type validatedOperation struct {
		model                   string
		fields                  modelMetadataFields
		thinking, input, output []string
	}
	validated := make([]validatedOperation, 0, len(body.Operations))
	seenModels := make(map[string]struct{}, len(body.Operations))
	for i, operation := range body.Operations {
		model := strings.TrimSpace(operation.Model)
		if model == "" {
			writeModelChannelError(c, channelError(http.StatusBadRequest, "operations[%d].model is empty", i))
			return
		}
		if _, exists := seenModels[model]; exists {
			writeModelChannelError(c, channelError(http.StatusConflict, "model %q appears more than once", model))
			return
		}
		seenModels[model] = struct{}{}
		if err := validateTokenPatch("max-context-length", operation.Fields.MaxContext); err != nil {
			writeModelChannelError(c, err)
			return
		}
		if err := validateTokenPatch("max-input-tokens", operation.Fields.MaxInput); err != nil {
			writeModelChannelError(c, err)
			return
		}
		if err := validateTokenPatch("max-output-tokens", operation.Fields.MaxOutput); err != nil {
			writeModelChannelError(c, err)
			return
		}
		thinking, err := validateStringSlicePatch("thinking.levels", operation.Fields.ThinkingLevels, false)
		if err != nil {
			writeModelChannelError(c, err)
			return
		}
		input, err := validateStringSlicePatch("input-modalities", operation.Fields.InputModalities, true)
		if err != nil {
			writeModelChannelError(c, err)
			return
		}
		output, err := validateStringSlicePatch("output-modalities", operation.Fields.OutputModalities, true)
		if err != nil {
			writeModelChannelError(c, err)
			return
		}
		if operation.Fields.ThinkingLevels == nil && operation.Fields.MaxContext == nil && operation.Fields.MaxInput == nil && operation.Fields.MaxOutput == nil && operation.Fields.InputModalities == nil && operation.Fields.OutputModalities == nil {
			writeModelChannelError(c, channelError(http.StatusBadRequest, "operations[%d].fields is empty", i))
			return
		}
		validated = append(validated, validatedOperation{model: model, fields: operation.Fields, thinking: thinking, input: input, output: output})
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "channel selector not found"})
		return
	}
	candidate := h.cfg.CloneForRuntime()
	var index int
	var selector modelChannelSelector
	var revision string
	var currentNames []string
	switch body.Kind {
	case modelChannelKindOpenAI:
		var entry *config.OpenAICompatibility
		var err error
		index, entry, selector, err = h.resolveOpenAIChannelLocked(body.Selector)
		if err != nil {
			writeModelChannelError(c, err)
			return
		}
		revision = openAIChannelRevision(entry, selector, h.cfg.ProxyURL)
		if entry.Disabled {
			c.JSON(http.StatusConflict, gin.H{"error": "channel is disabled"})
			return
		}
		currentNames = modelNamesOpenAI(entry.Models)
		if err := rejectDuplicateModelNames(currentNames); err != nil {
			writeModelChannelError(c, err)
			return
		}
		positions := make(map[string]int, len(entry.Models))
		for i, model := range entry.Models {
			positions[model.Name] = i
		}
		for _, operation := range validated {
			position, found := positions[operation.model]
			if !found {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", operation.model)})
				return
			}
			model := &candidate.OpenAICompatibility[index].Models[position]
			patchThinking(&model.Thinking, operation.fields.ThinkingLevels, operation.thinking)
			patchInt(&model.MaxContextLength, operation.fields.MaxContext)
			patchInt(&model.MaxInputTokens, operation.fields.MaxInput)
			patchInt(&model.MaxOutputTokens, operation.fields.MaxOutput)
			patchSlice(&model.InputModalities, operation.fields.InputModalities, operation.input)
			patchSlice(&model.OutputModalities, operation.fields.OutputModalities, operation.output)
		}
	case modelChannelKindClaude:
		var entry *config.ClaudeKey
		var err error
		index, entry, selector, err = h.resolveClaudeChannelLocked(body.Selector)
		if err != nil {
			writeModelChannelError(c, err)
			return
		}
		revision = claudeChannelRevision(entry, selector, h.cfg.ProxyURL)
		currentNames = modelNamesClaude(entry.Models)
		if err := rejectDuplicateModelNames(currentNames); err != nil {
			writeModelChannelError(c, err)
			return
		}
		positions := make(map[string]int, len(entry.Models))
		for i, model := range entry.Models {
			positions[model.Name] = i
		}
		for _, operation := range validated {
			position, found := positions[operation.model]
			if !found {
				c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("model %q not found", operation.model)})
				return
			}
			model := &candidate.ClaudeKey[index].Models[position]
			patchThinking(&model.Thinking, operation.fields.ThinkingLevels, operation.thinking)
			patchInt(&model.MaxContextLength, operation.fields.MaxContext)
			patchInt(&model.MaxInputTokens, operation.fields.MaxInput)
			patchInt(&model.MaxOutputTokens, operation.fields.MaxOutput)
			patchSlice(&model.InputModalities, operation.fields.InputModalities, operation.input)
			patchSlice(&model.OutputModalities, operation.fields.OutputModalities, operation.output)
		}
	default:
		writeModelChannelError(c, channelError(http.StatusBadRequest, "unsupported channel kind %q", body.Kind))
		return
	}
	if body.ExpectedRevision != revision || !equalStrings(body.ExpectedModelNames, currentNames) {
		c.JSON(http.StatusConflict, gin.H{"error": "channel revision or model-set precondition drifted"})
		return
	}
	if err := h.persistModelChannelCandidateLocked(candidate, body.Kind, index, selector, body.ExpectedRevision, body.ExpectedModelNames); err != nil {
		writeModelChannelError(c, err)
		return
	}
	if body.Kind == modelChannelKindOpenAI {
		h.cfg.OpenAICompatibility[index].Models = candidate.OpenAICompatibility[index].Models
		revision = openAIChannelRevision(&h.cfg.OpenAICompatibility[index], selector, h.cfg.ProxyURL)
	} else {
		h.cfg.ClaudeKey[index].Models = candidate.ClaudeKey[index].Models
		revision = claudeChannelRevision(&h.cfg.ClaudeKey[index], selector, h.cfg.ProxyURL)
	}
	snapshot := h.reloadSnapshotConfigLocked()
	c.JSON(http.StatusOK, gin.H{"status": "ok", "revision": revision})
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
}

func verifyDiskModelChannel(disk *config.Config, kind string, index int, selector modelChannelSelector, expectedRevision string, expectedModelNames []string) error {
	if disk == nil {
		return channelError(http.StatusConflict, "config file changed")
	}
	switch kind {
	case modelChannelKindOpenAI:
		if index < 0 || index >= len(disk.OpenAICompatibility) {
			return channelError(http.StatusConflict, "config file channel order drifted")
		}
		entry := &disk.OpenAICompatibility[index]
		baseURL, err := canonicalizeModelChannelURL(entry.BaseURL)
		if err != nil || strings.TrimSpace(entry.Name) != selector.Name || baseURL != selector.BaseURL {
			return channelError(http.StatusConflict, "config file channel selector drifted")
		}
		names := modelNamesOpenAI(entry.Models)
		if err := rejectDuplicateModelNames(names); err != nil {
			return err
		}
		if openAIChannelRevision(entry, selector, disk.ProxyURL) != expectedRevision || !equalStrings(names, expectedModelNames) {
			return channelError(http.StatusConflict, "config file channel revision or model set drifted")
		}
	case modelChannelKindClaude:
		if index < 0 || index >= len(disk.ClaudeKey) {
			return channelError(http.StatusConflict, "config file channel order drifted")
		}
		entry := &disk.ClaudeKey[index]
		baseURL, err := canonicalizeModelChannelURL(effectiveClaudeBaseURL(entry.BaseURL))
		if err != nil || selector.ConfigIndex == nil || *selector.ConfigIndex != index || baseURL != selector.BaseURL || normalizeChannelPrefix(entry.Prefix) != selector.Prefix {
			return channelError(http.StatusConflict, "config file channel selector drifted")
		}
		names := modelNamesClaude(entry.Models)
		if err := rejectDuplicateModelNames(names); err != nil {
			return err
		}
		if claudeChannelRevision(entry, selector, disk.ProxyURL) != expectedRevision || !equalStrings(names, expectedModelNames) {
			return channelError(http.StatusConflict, "config file channel revision or model set drifted")
		}
	default:
		return channelError(http.StatusBadRequest, "unsupported channel kind %q", kind)
	}
	return nil
}

// persistModelChannelCandidateLocked keeps the config inode stable. Crash recovery uses
// <config>.model-channel.bak; see docs/model-channel-management.md.
func (h *Handler) persistModelChannelCandidateLocked(candidate *config.Config, kind string, index int, selector modelChannelSelector, expectedRevision string, expectedModelNames []string) error {
	if h == nil || h.cfg == nil || candidate == nil {
		return channelError(http.StatusInternalServerError, "model channel config is unavailable")
	}
	if strings.TrimSpace(h.configFilePath) == "" {
		return channelError(http.StatusInternalServerError, "config file path is unavailable")
	}
	oldBytes, err := os.ReadFile(h.configFilePath)
	if err != nil {
		return channelError(http.StatusInternalServerError, "backup current config: %v", err)
	}
	disk, err := config.ParseConfigBytes(oldBytes)
	if err != nil {
		return channelError(http.StatusConflict, "config file changed or is invalid")
	}
	if err = verifyDiskModelChannel(disk, kind, index, selector, expectedRevision, expectedModelNames); err != nil {
		return err
	}
	if err = writeModelChannelBackup(h.configFilePath+".model-channel.bak", oldBytes); err != nil {
		return channelError(http.StatusInternalServerError, "persist recovery backup: %v", err)
	}
	updated, err := renderModelChannelConfig(oldBytes, candidate, kind, index)
	if err != nil {
		return channelError(http.StatusInternalServerError, "%v", err)
	}
	if _, err = config.ParseConfigBytes(updated); err != nil {
		return channelError(http.StatusInternalServerError, "validate rendered config: %v", err)
	}
	if err = modelChannelWriteInPlace(h.configFilePath, updated); err != nil {
		if rollbackErr := modelChannelWriteInPlace(h.configFilePath, oldBytes); rollbackErr != nil {
			return channelError(http.StatusInternalServerError, "write config: %v; rollback failed: %v", err, rollbackErr)
		}
		return channelError(http.StatusInternalServerError, "write config: %v", err)
	}
	return nil
}

func renderModelChannelConfig(oldBytes []byte, candidate *config.Config, kind string, index int) ([]byte, error) {
	var original yaml.Node
	if err := yaml.Unmarshal(oldBytes, &original); err != nil {
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	if original.Kind != yaml.DocumentNode || len(original.Content) == 0 || original.Content[0] == nil || original.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("invalid config yaml document")
	}
	section := "openai-compatibility"
	var models any
	if kind == modelChannelKindOpenAI {
		if index < 0 || index >= len(candidate.OpenAICompatibility) {
			return nil, fmt.Errorf("openai channel index out of range")
		}
		models = candidate.OpenAICompatibility[index].Models
	} else if kind == modelChannelKindClaude {
		section = "claude-api-key"
		if index < 0 || index >= len(candidate.ClaudeKey) {
			return nil, fmt.Errorf("claude channel index out of range")
		}
		models = candidate.ClaudeKey[index].Models
	} else {
		return nil, fmt.Errorf("unsupported channel kind")
	}
	sequence := yamlMappingValue(original.Content[0], section)
	if sequence == nil || sequence.Kind != yaml.SequenceNode || index >= len(sequence.Content) || sequence.Content[index] == nil || sequence.Content[index].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("selected channel yaml node not found")
	}
	modelsNode, err := buildTargetModelsYAMLNode(sequence.Content[index], models)
	if err != nil {
		return nil, err
	}
	setYAMLMappingValue(sequence.Content[index], "models", modelsNode)
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err = encoder.Encode(&original); err != nil {
		_ = encoder.Close()
		return nil, fmt.Errorf("encode config yaml: %w", err)
	}
	if err = encoder.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return config.NormalizeCommentIndentation(out.Bytes()), nil
}

func buildTargetModelsYAMLNode(channelNode *yaml.Node, models any) (*yaml.Node, error) {
	rendered, err := yaml.Marshal(models)
	if err != nil {
		return nil, fmt.Errorf("marshal models: %w", err)
	}
	var modelsDoc yaml.Node
	if err = yaml.Unmarshal(rendered, &modelsDoc); err != nil || len(modelsDoc.Content) == 0 {
		return nil, fmt.Errorf("build models yaml node: %w", err)
	}
	generated := modelsDoc.Content[0]
	existing := yamlMappingValue(channelNode, "models")
	if generated == nil || generated.Kind != yaml.SequenceNode || existing == nil || existing.Kind != yaml.SequenceNode {
		return generated, nil
	}
	byName := make(map[string]*yaml.Node, len(existing.Content))
	for _, node := range existing.Content {
		if name := yamlModelName(node); name != "" {
			if _, duplicate := byName[name]; !duplicate {
				byName[name] = node
			}
		}
	}
	out := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: existing.Style}
	for _, generatedModel := range generated.Content {
		name := yamlModelName(generatedModel)
		if original := byName[name]; original != nil {
			mergeKnownYAMLMappingFields(original, generatedModel)
			out.Content = append(out.Content, original)
			continue
		}
		out.Content = append(out.Content, generatedModel)
	}
	return out, nil
}

func yamlModelName(node *yaml.Node) string {
	if value := yamlMappingValue(node, "name"); value != nil && value.Kind == yaml.ScalarNode {
		return value.Value
	}
	return ""
}

func mergeKnownYAMLMappingFields(destination, generated *yaml.Node) {
	if destination == nil || generated == nil || destination.Kind != yaml.MappingNode || generated.Kind != yaml.MappingNode {
		return
	}
	known := map[string]struct{}{
		"name": {}, "alias": {}, "display-name": {}, "max-context-length": {}, "max-input-tokens": {}, "max-output-tokens": {},
		"force-mapping": {}, "image": {}, "input-modalities": {}, "output-modalities": {}, "is-compat": {}, "thinking": {},
	}
	for key := range known {
		value := yamlMappingValue(generated, key)
		if value == nil {
			removeYAMLMappingValue(destination, key)
			continue
		}
		setYAMLMappingValue(destination, key, value)
	}
}

func removeYAMLMappingValue(node *yaml.Node, key string) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i] != nil && node.Content[i].Value == key {
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			return
		}
	}
}

func writeModelChannelBackup(path string, data []byte) error {
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if errClose := file.Close(); err == nil {
		err = errClose
	}
	if err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func writeModelChannelFileInPlace(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Debug("model channel config close failed")
		}
	}()
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	if err = file.Truncate(int64(len(data))); err != nil {
		return err
	}
	return file.Sync()
}
