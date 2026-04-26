package backend

// CodeWhispererBackend implements Backend for Amazon CodeWhisperer / Kiro.
//
// COVERAGE: Intentionally untested. Reverse-engineered Kiro streaming API
// client requiring a live session to test.
//
//
// Auth is configured via the apiKey parameter to NewCodeWhisperer:
//   - Empty string → read from Kiro CLI SQLite database at the default path
//     ($XDG_DATA_HOME/kiro-cli/data.sqlite3 on Linux).
//   - "sqlite:///path/to/data.sqlite3" → read from the specified SQLite file.
//   - Any other string → used directly as a bearer token (static auth).
//
// SQLite auth requires the sqlite3 CLI to be present in PATH.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ── Public constructor ────────────────────────────────────────────────────────

type CodeWhispererBackend struct {
	endpoint     string // overrides auth-derived endpoint if non-empty
	model        string
	extraHeaders map[string]string
	authSource   kiroAuthSource
	authInitErr  error
	httpClient   *http.Client
	ctxLength    int // cached
}

// NewCodeWhisperer returns a CodeWhisperer backend. See package doc for apiKey semantics.
func NewCodeWhisperer(apiKey string) (*CodeWhispererBackend, error) {
	if strings.TrimSpace(apiKey) == "" {
		apiKey = kiroDefaultSQLiteKey()
	}
	authSource, err := newKiroAuthSource(apiKey)
	b := &CodeWhispererBackend{
		authSource:  authSource,
		authInitErr: err,
		httpClient:  &http.Client{},
	}
	b.model = b.DefaultModel()
	return b, nil
}

func (b *CodeWhispererBackend) Name() string         { return "kiro" }
func (b *CodeWhispererBackend) DefaultModel() string { return "auto" }
func (b *CodeWhispererBackend) Model() string        { return b.model }
func (b *CodeWhispererBackend) SetModel(m string)    { b.model = m }
func (b *CodeWhispererBackend) Models(ctx context.Context) []string {
	resp := b.fetchModels(ctx)
	if resp == nil {
		return nil
	}
	ids := make([]string, len(resp.Models))
	for i, m := range resp.Models {
		ids[i] = m.ModelID
	}
	return ids
}

func (b *CodeWhispererBackend) fetchModels(ctx context.Context) *kiroListModelsResponse {
	if b.authInitErr != nil {
		return nil
	}
	token, err := b.authSource.AccessToken(ctx)
	if err != nil {
		return nil
	}
	endpoint, err := b.resolveEndpoint(ctx)
	if err != nil {
		return nil
	}
	profileARN, _ := b.authSource.ProfileARN(ctx)
	client := newKiroAPIClient(endpoint, token, b.extraHeaders, b.httpClient)
	resp, err := client.ListModels(ctx, profileARN)
	if err != nil {
		return nil
	}
	return resp
}

func (b *CodeWhispererBackend) ContextLength(ctx context.Context) int {
	if b.ctxLength > 0 {
		return b.ctxLength
	}
	resp := b.fetchModels(ctx)
	if resp == nil {
		return 0
	}
	for _, m := range resp.Models {
		if m.TokenLimits != nil && (b.model == "auto" || m.ModelID == b.model) {
			b.ctxLength = m.TokenLimits.MaxInputTokens
			return b.ctxLength
		}
	}
	if resp.DefaultModel != nil && resp.DefaultModel.TokenLimits != nil {
		b.ctxLength = resp.DefaultModel.TokenLimits.MaxInputTokens
		return b.ctxLength
	}
	return 0
}

// ── Backend interface ─────────────────────────────────────────────────────────

func (b *CodeWhispererBackend) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []Tool,
	params GenerationParams,
) (<-chan StreamEvent, error) {
	model := b.model
	if b.authInitErr != nil {
		return nil, b.authInitErr
	}

	profileARN, err := b.authSource.ProfileARN(ctx)
	if err != nil {
		return nil, fmt.Errorf("kiro profile ARN: %w", err)
	}

	req, err := buildKiroRequest(model, messages, tools, profileARN)
	if err != nil {
		return nil, err
	}

	ch := make(chan StreamEvent, 8)
	go func() {
		defer close(ch)
		b.runStream(ctx, req, ch)
	}()
	return ch, nil
}

func (b *CodeWhispererBackend) runStream(ctx context.Context, req *kiroGenerateRequest, ch chan<- StreamEvent) {
	for attempt := range 2 {
		token, err := b.authSource.AccessToken(ctx)
		if err != nil {
			ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("auth: %v", err)}
			return
		}
		endpoint, err := b.resolveEndpoint(ctx)
		if err != nil {
			ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("endpoint: %v", err)}
			return
		}

		client := newKiroAPIClient(endpoint, token, b.extraHeaders, b.httpClient)

		stopped := false
		result, streamErr := client.Stream(ctx, *req, kiroStreamCallbacks{
			OnAssistantDelta: func(event kiroAssistantEvent) {
				if stopped || event.Content == "" {
					return
				}
				select {
				case ch <- StreamEvent{Content: event.Content}:
				case <-ctx.Done():
					stopped = true
				}
			},
		})

		if streamErr != nil {
			if attempt == 0 && b.shouldRefresh(streamErr) {
				if refreshErr := b.authSource.Refresh(ctx); refreshErr != nil {
					ch <- StreamEvent{Done: true, StopReason: fmt.Sprintf("token refresh: %v", refreshErr)}
					return
				}
				continue
			}
			ch <- StreamEvent{Done: true, StopReason: streamErr.Error()}
			return
		}

		final := StreamEvent{Done: true, StopReason: "stop"}
		if len(result.ToolUses) > 0 {
			final.StopReason = "tool_calls"
			for _, tu := range result.ToolUses {
				final.ToolCalls = append(final.ToolCalls, ToolCall{
					ID:        tu.ToolUseID,
					Name:      tu.Name,
					Arguments: tu.Input,
				})
			}
		}
		ch <- final
		return
	}
}

func (b *CodeWhispererBackend) resolveEndpoint(ctx context.Context) (string, error) {
	if ep := strings.TrimSpace(b.endpoint); ep != "" {
		return ep, nil
	}
	return b.authSource.DefaultEndpoint(ctx)
}

func (b *CodeWhispererBackend) shouldRefresh(err error) bool {
	if !b.authSource.CanRefresh() {
		return false
	}
	var apiErr *kiroAPIError
	return errors.As(err, &apiErr) && apiErr.isUnauthorized()
}

// ── Request building ──────────────────────────────────────────────────────────

// kiroSanitizeModel returns "auto" for any model string that isn't a native
// Kiro/CodeWhisperer model (e.g. Ollama "name:tag" or OpenRouter "org/name").
func kiroSanitizeModel(model string) string {
	if model == "" || strings.ContainsAny(model, "/:") {
		return "auto"
	}
	return model
}

func buildKiroRequest(model string, messages []Message, tools []Tool, profileARN string) (*kiroGenerateRequest, error) {
	model = kiroSanitizeModel(model)
	if len(messages) == 0 {
		return nil, fmt.Errorf("at least one message is required")
	}

	encodedTools := encodeKiroToolDefinitions(tools)

	lastRole := strings.ToLower(strings.TrimSpace(messages[len(messages)-1].Role))

	var history []kiroChatMessage
	var current kiroChatMessage
	var err error

	switch lastRole {
	case "user", "system":
		history, err = encodeKiroHistory(messages[:len(messages)-1])
		if err != nil {
			return nil, err
		}
		current = kiroChatMessage{UserInputMessage: encodeKiroUserMessage(messages[len(messages)-1], model, encodedTools, nil)}

	case "tool":
		// Find the last non-tool message.
		cutoff := len(messages) - 1
		for cutoff >= 0 && strings.EqualFold(messages[cutoff].Role, "tool") {
			cutoff--
		}
		history, err = encodeKiroHistory(messages[:cutoff+1])
		if err != nil {
			return nil, err
		}
		toolResults := encodeKiroToolResults(messages[cutoff+1:])
		current = kiroChatMessage{UserInputMessage: emptyKiroUserMessage(model, encodedTools, toolResults)}

	default:
		return nil, fmt.Errorf("last message role %q is not supported (expected user, system, or tool)", lastRole)
	}

	convID, err := kiroNewUUID()
	if err != nil {
		return nil, err
	}
	contID, err := kiroNewUUID()
	if err != nil {
		return nil, err
	}

	return &kiroGenerateRequest{
		ProfileARN: profileARN,
		ConversationState: kiroConversationState{
			ConversationID:      convID,
			History:             history,
			CurrentMessage:      current,
			ChatTriggerType:     kiroTriggerType,
			AgentContinuationID: contID,
			AgentTaskType:       kiroAgentTaskType,
		},
	}, nil
}

func encodeKiroHistory(messages []Message) ([]kiroChatMessage, error) {
	out := make([]kiroChatMessage, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "user", "system":
			out = append(out, kiroChatMessage{UserInputMessage: encodeKiroUserMessage(m, "", nil, nil)})
		case "assistant":
			out = append(out, kiroChatMessage{AssistantResponseMessage: encodeKiroAssistantMessage(m)})
		case "tool":
			// Batch consecutive tool messages together.
			start := i
			for i+1 < len(messages) && strings.EqualFold(messages[i+1].Role, "tool") {
				i++
			}
			results := encodeKiroToolResults(messages[start : i+1])
			out = append(out, kiroChatMessage{UserInputMessage: emptyKiroUserMessage("", nil, results)})
		}
	}
	return out, nil
}

func encodeKiroUserMessage(m Message, modelID string, tools []kiroTool, toolResults []kiroToolResult) *kiroUserInputMessage {
	msg := &kiroUserInputMessage{
		Content: m.Content,
		Origin:  kiroDefaultOrigin,
		ModelID: modelID,
	}
	ctx := &kiroUserInputContext{}
	if len(tools) > 0 {
		ctx.Tools = tools
	}
	if len(toolResults) > 0 {
		ctx.ToolResults = toolResults
	}
	if len(ctx.Tools) > 0 || len(ctx.ToolResults) > 0 {
		msg.UserInputMessageContext = ctx
	}
	return msg
}

func emptyKiroUserMessage(modelID string, tools []kiroTool, toolResults []kiroToolResult) *kiroUserInputMessage {
	msg := &kiroUserInputMessage{
		Content: "",
		Origin:  kiroDefaultOrigin,
		ModelID: modelID,
	}
	ctx := &kiroUserInputContext{}
	if len(tools) > 0 {
		ctx.Tools = tools
	}
	if len(toolResults) > 0 {
		ctx.ToolResults = toolResults
	}
	if len(ctx.Tools) > 0 || len(ctx.ToolResults) > 0 {
		msg.UserInputMessageContext = ctx
	}
	return msg
}

func encodeKiroAssistantMessage(m Message) *kiroAssistantResponseMessage {
	msg := &kiroAssistantResponseMessage{Content: m.Content}
	for _, tc := range m.ToolCalls {
		input := tc.Arguments
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		toolUseID := strings.TrimSpace(tc.ID)
		if toolUseID == "" {
			toolUseID, _ = kiroNewUUID()
		}
		msg.ToolUses = append(msg.ToolUses, kiroToolUse{
			ToolUseID: toolUseID,
			Name:      tc.Name,
			Input:     input,
		})
	}
	return msg
}

func encodeKiroToolDefinitions(tools []Tool) []kiroTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]kiroTool, 0, len(tools))
	for _, t := range tools {
		schema := t.Parameters
		if schema == nil {
			schema = json.RawMessage("{}")
		}
		out = append(out, kiroTool{
			ToolSpecification: &kiroToolSpecification{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: kiroInputSchema{JSON: schema},
			},
		})
	}
	return out
}

func encodeKiroToolResults(messages []Message) []kiroToolResult {
	results := make([]kiroToolResult, 0, len(messages))
	for _, m := range messages {
		if !strings.EqualFold(m.Role, "tool") {
			continue
		}
		toolUseID := strings.TrimSpace(m.ToolCallID)
		results = append(results, kiroToolResult{
			ToolUseID: toolUseID,
			Content:   []kiroToolResultContent{encodeKiroToolResultContent(m.Content)},
		})
	}
	return results
}

func encodeKiroToolResultContent(content string) kiroToolResultContent {
	content = strings.TrimSpace(content)
	if len(content) > 0 && content[0] == '{' && json.Valid([]byte(content)) {
		return kiroToolResultContent{JSON: json.RawMessage(content)}
	}
	return kiroToolResultContent{Text: content}
}

// ── Auth sources ──────────────────────────────────────────────────────────────

type kiroAuthSource interface {
	DefaultEndpoint(ctx context.Context) (string, error)
	ProfileARN(ctx context.Context) (string, error)
	AccessToken(ctx context.Context) (string, error)
	Refresh(ctx context.Context) error
	CanRefresh() bool
}

// staticKiroAuth holds a pre-issued bearer token.
type staticKiroAuth struct {
	token string
}

func (s *staticKiroAuth) DefaultEndpoint(_ context.Context) (string, error) {
	return "", fmt.Errorf("base URL not configured and no profile ARN available with static auth")
}

func (s *staticKiroAuth) ProfileARN(_ context.Context) (string, error) { return "", nil }

func (s *staticKiroAuth) AccessToken(_ context.Context) (string, error) {
	t := strings.TrimSpace(s.token)
	if t == "" {
		return "", fmt.Errorf("token is empty")
	}
	return t, nil
}

func (s *staticKiroAuth) Refresh(_ context.Context) error {
	return fmt.Errorf("static tokens cannot be refreshed")
}

func (s *staticKiroAuth) CanRefresh() bool { return false }

// sqliteKiroAuth reads tokens from the Kiro CLI SQLite database and refreshes via OIDC.
type sqliteKiroAuth struct {
	path   string
	state  kiroSQLiteState
	loaded bool
	initMu sync.Mutex
	mu     sync.Mutex
}

type kiroSQLiteState struct {
	ProfileARN   string // from OIDC profile state; empty for social/personal accounts
	Region       string
	AccessToken  string
	RefreshToken string
	ExpiresAt    string
	ClientID     string
	ClientSecret string
	IsSocial     bool // true for personal (GitHub/Google) accounts
}

func (s *sqliteKiroAuth) DefaultEndpoint(ctx context.Context) (string, error) {
	state, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if arn := strings.TrimSpace(state.ProfileARN); arn != "" {
		return kiroDefaultEndpoint(arn)
	}
	if region := strings.TrimSpace(state.Region); region != "" {
		return kiroEndpointForRegion(region), nil
	}
	return "", fmt.Errorf("cannot derive endpoint: no profile ARN or region in Kiro auth state")
}

func (s *sqliteKiroAuth) ProfileARN(ctx context.Context) (string, error) {
	state, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	// Personal/social accounts don't send profileArn in requests.
	if state.IsSocial {
		return "", nil
	}
	return strings.TrimSpace(state.ProfileARN), nil
}

func (s *sqliteKiroAuth) AccessToken(ctx context.Context) (string, error) {
	state, err := s.load(ctx)
	if err != nil {
		return "", err
	}
	if state.AccessToken != "" && !kiroTokenExpiringSoon(state.ExpiresAt) {
		return state.AccessToken, nil
	}
	if !s.canRefreshState(state) {
		if state.AccessToken != "" {
			return state.AccessToken, nil // expired but can't refresh — use it anyway
		}
		return "", fmt.Errorf("sqlite auth state %q: no access token and cannot refresh", s.path)
	}
	if err := s.Refresh(ctx); err != nil {
		if state.AccessToken != "" {
			return state.AccessToken, nil // refresh failed — use stale token
		}
		return "", err
	}
	state, err = s.load(ctx)
	if err != nil {
		return "", err
	}
	return state.AccessToken, nil
}

func (s *sqliteKiroAuth) Refresh(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.load(ctx)
	if err != nil {
		return err
	}
	if !s.canRefreshState(state) {
		return fmt.Errorf("sqlite auth state %q: insufficient data for token refresh", s.path)
	}

	// Re-read current token — another process may have already refreshed it.
	currentAccess, currentRefresh, currentExpiry, err := kiroReadSQLiteTokens(ctx, s.path)
	if err == nil && currentAccess != "" && currentAccess != state.AccessToken && !kiroTokenExpiringSoon(currentExpiry) {
		return nil // already refreshed by another process
	}

	refreshToken := currentRefresh
	if refreshToken == "" {
		refreshToken = state.RefreshToken
	}

	if state.IsSocial {
		return s.refreshSocial(ctx, state, refreshToken)
	}
	return s.refreshOIDC(ctx, state, refreshToken)
}

func (s *sqliteKiroAuth) refreshOIDC(ctx context.Context, state kiroSQLiteState, refreshToken string) error {
	oidcEndpoint := fmt.Sprintf("https://oidc.%s.amazonaws.com/token", strings.TrimSpace(state.Region))
	client := newKiroOIDCClient(oidcEndpoint, nil)

	resp, err := client.RefreshToken(ctx, kiroRefreshTokenRequest{
		ClientID:     state.ClientID,
		ClientSecret: state.ClientSecret,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
	if err != nil {
		return err
	}

	newAccess := strings.TrimSpace(resp.AccessToken)
	newRefresh := strings.TrimSpace(resp.RefreshToken)
	var expiresAt string
	if resp.ExpiresIn != nil && *resp.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(*resp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	return kiroUpdateSQLiteTokens(ctx, s.path, newAccess, newRefresh, expiresAt)
}

func (s *sqliteKiroAuth) refreshSocial(ctx context.Context, state kiroSQLiteState, refreshToken string) error {
	client := newKiroSocialAuthClient(nil)
	resp, err := client.RefreshToken(ctx, refreshToken)
	if err != nil {
		return err
	}

	newAccess := strings.TrimSpace(resp.AccessToken)
	newRefresh := strings.TrimSpace(resp.RefreshToken)
	var expiresAt string
	if resp.ExpiresIn != nil && *resp.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(*resp.ExpiresIn) * time.Second).UTC().Format(time.RFC3339Nano)
	}
	profileARN := strings.TrimSpace(resp.ProfileARN)
	if profileARN == "" {
		profileARN = state.ProfileARN
	}
	return kiroUpdateSQLiteSocialTokens(ctx, s.path, newAccess, newRefresh, expiresAt, profileARN)
}

func (s *sqliteKiroAuth) CanRefresh() bool {
	state, err := s.load(context.Background())
	return err == nil && s.canRefreshState(state)
}

func (s *sqliteKiroAuth) canRefreshState(state kiroSQLiteState) bool {
	if strings.TrimSpace(state.RefreshToken) == "" {
		return false
	}
	if state.IsSocial {
		return true
	}
	return strings.TrimSpace(state.Region) != "" &&
		strings.TrimSpace(state.ClientID) != "" &&
		strings.TrimSpace(state.ClientSecret) != ""
}

func (s *sqliteKiroAuth) load(ctx context.Context) (kiroSQLiteState, error) {
	s.initMu.Lock()
	if !s.loaded {
		state, err := kiroLoadSQLiteState(ctx, s.path)
		if err != nil {
			s.initMu.Unlock()
			return kiroSQLiteState{}, err
		}
		s.state = state
		s.loaded = true
	}
	s.initMu.Unlock()

	// Always re-read tokens — they may have been refreshed by another process.
	access, refresh, expiresAt, err := kiroReadSQLiteTokens(ctx, s.path)
	if err != nil {
		return s.state, nil // fall back to cached state
	}
	state := s.state
	state.AccessToken = access
	state.ExpiresAt = expiresAt
	if refresh != "" {
		state.RefreshToken = refresh
	}
	return state, nil
}

const kiroTokenExpiryBuffer = 2 * time.Minute

func kiroTokenExpiringSoon(expiresAt string) bool {
	if expiresAt == "" {
		return false
	}
	// Social auth uses nanosecond precision; OIDC auth uses second precision.
	t, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		t, err = time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return false
		}
	}
	return time.Until(t) < kiroTokenExpiryBuffer
}

// ── SQLite helpers ────────────────────────────────────────────────────────────

// Kiro supports two auth flows whose tokens live under different SQLite keys:
//   - OIDC (org/enterprise accounts): kirocli:odic:token + kirocli:odic:device-registration
//   - Social (personal accounts, e.g. GitHub login): kirocli:social:token
//
// We probe OIDC first; if that key is absent we fall back to social.

const (
	kiroSQLiteProfileQuery      = "SELECT value FROM state WHERE key = 'api.codewhisperer.profile' LIMIT 1;"
	kiroSQLiteOIDCTokenQuery    = "SELECT value FROM auth_kv WHERE key = 'kirocli:odic:token' LIMIT 1;"
	kiroSQLiteDeviceRegQuery    = "SELECT value FROM auth_kv WHERE key = 'kirocli:odic:device-registration' LIMIT 1;"
	kiroSQLiteSocialTokenQuery  = "SELECT value FROM auth_kv WHERE key = 'kirocli:social:token' LIMIT 1;"
)

type kiroSQLiteProfileState struct {
	ARN string `json:"arn"`
}

// OIDC auth token (org/enterprise accounts)
type kiroSQLiteOIDCTokenState struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	Region       string `json:"region"`
}

type kiroSQLiteDeviceRegState struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Region       string `json:"region"`
}

// Social auth token (personal accounts — GitHub, etc.)
type kiroSQLiteSocialTokenState struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	ProfileARN   string `json:"profile_arn"`
}

func kiroLoadSQLiteState(ctx context.Context, path string) (kiroSQLiteState, error) {
	// Try OIDC token first.
	if oidcVal, ok, err := kiroQuerySQLiteOptional(ctx, path, kiroSQLiteOIDCTokenQuery); err != nil {
		return kiroSQLiteState{}, err
	} else if ok {
		return kiroLoadOIDCState(ctx, path, oidcVal)
	}

	// Fall back to social token (personal accounts).
	socialVal, err := kiroQuerySQLite(ctx, path, kiroSQLiteSocialTokenQuery)
	if err != nil {
		return kiroSQLiteState{}, fmt.Errorf("no OIDC or social token found in %q", path)
	}
	return kiroLoadSocialState(socialVal)
}

func kiroLoadOIDCState(ctx context.Context, path, tokenVal string) (kiroSQLiteState, error) {
	var token kiroSQLiteOIDCTokenState
	if err := json.Unmarshal([]byte(tokenVal), &token); err != nil {
		return kiroSQLiteState{}, fmt.Errorf("decode OIDC token from sqlite %q: %w", path, err)
	}

	var profile kiroSQLiteProfileState
	if v, ok, err := kiroQuerySQLiteOptional(ctx, path, kiroSQLiteProfileQuery); err != nil {
		return kiroSQLiteState{}, err
	} else if ok {
		_ = json.Unmarshal([]byte(v), &profile)
	}

	var dev kiroSQLiteDeviceRegState
	if devVal, ok, err := kiroQuerySQLiteOptional(ctx, path, kiroSQLiteDeviceRegQuery); err != nil {
		return kiroSQLiteState{}, err
	} else if ok {
		_ = json.Unmarshal([]byte(devVal), &dev)
	}

	region := strings.TrimSpace(token.Region)
	if region == "" {
		region = strings.TrimSpace(dev.Region)
	}

	return kiroSQLiteState{
		ProfileARN:   strings.TrimSpace(profile.ARN),
		Region:       region,
		AccessToken:  strings.TrimSpace(token.AccessToken),
		RefreshToken: strings.TrimSpace(token.RefreshToken),
		ExpiresAt:    strings.TrimSpace(token.ExpiresAt),
		ClientID:     strings.TrimSpace(dev.ClientID),
		ClientSecret: strings.TrimSpace(dev.ClientSecret),
	}, nil
}

func kiroLoadSocialState(tokenVal string) (kiroSQLiteState, error) {
	var token kiroSQLiteSocialTokenState
	if err := json.Unmarshal([]byte(tokenVal), &token); err != nil {
		return kiroSQLiteState{}, fmt.Errorf("decode social token: %w", err)
	}
	return kiroSQLiteState{
		ProfileARN:   strings.TrimSpace(token.ProfileARN), // used only for endpoint derivation
		AccessToken:  strings.TrimSpace(token.AccessToken),
		RefreshToken: strings.TrimSpace(token.RefreshToken),
		ExpiresAt:    strings.TrimSpace(token.ExpiresAt),
		IsSocial:     true,
		// No Region, ClientID, ClientSecret — social auth can't use OIDC refresh.
	}, nil
}

// kiroReadSQLiteTokens re-reads the current tokens from SQLite (for cross-process
// refresh detection). Tries OIDC key first, then social.
func kiroReadSQLiteTokens(ctx context.Context, path string) (access, refresh, expiresAt string, err error) {
	// Try OIDC token.
	if out, err2 := kiroRunSQLite(ctx, "-readonly", "-batch", "-noheader", path, kiroSQLiteOIDCTokenQuery); err2 == nil {
		if value := strings.TrimSpace(string(out)); value != "" {
			var state kiroSQLiteOIDCTokenState
			if err2 := json.Unmarshal([]byte(value), &state); err2 == nil {
				return strings.TrimSpace(state.AccessToken), strings.TrimSpace(state.RefreshToken), strings.TrimSpace(state.ExpiresAt), nil
			}
		}
	}

	// Fall back to social token.
	out, err2 := kiroRunSQLite(ctx, "-readonly", "-batch", "-noheader", path, kiroSQLiteSocialTokenQuery)
	if err2 != nil {
		return "", "", "", fmt.Errorf("read token from sqlite %q: %w", path, err2)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", "", "", fmt.Errorf("read token from sqlite %q: no rows", path)
	}
	var state kiroSQLiteSocialTokenState
	if err2 := json.Unmarshal([]byte(value), &state); err2 != nil {
		return "", "", "", fmt.Errorf("decode social token from sqlite %q: %w", path, err2)
	}
	return strings.TrimSpace(state.AccessToken), strings.TrimSpace(state.RefreshToken), strings.TrimSpace(state.ExpiresAt), nil
}

func kiroUpdateSQLiteTokens(ctx context.Context, path, access, refresh, expiresAt string) error {
	access = strings.TrimSpace(access)
	if access == "" {
		return fmt.Errorf("access token must not be empty")
	}
	var sb strings.Builder
	sb.WriteString("BEGIN IMMEDIATE;\n")
	sb.WriteString("UPDATE auth_kv SET value = json_set(value, '$.access_token', ")
	sb.WriteString(kiroSQLiteQuote(access))
	if r := strings.TrimSpace(refresh); r != "" {
		sb.WriteString(", '$.refresh_token', ")
		sb.WriteString(kiroSQLiteQuote(r))
	}
	if e := strings.TrimSpace(expiresAt); e != "" {
		sb.WriteString(", '$.expires_at', ")
		sb.WriteString(kiroSQLiteQuote(e))
	}
	sb.WriteString(") WHERE key = 'kirocli:odic:token';\nCOMMIT;\n")
	_, err := kiroRunSQLite(ctx, "-batch", path, sb.String())
	return err
}

func kiroUpdateSQLiteSocialTokens(ctx context.Context, path, access, refresh, expiresAt, profileARN string) error {
	access = strings.TrimSpace(access)
	if access == "" {
		return fmt.Errorf("access token must not be empty")
	}
	var sb strings.Builder
	sb.WriteString("BEGIN IMMEDIATE;\n")
	sb.WriteString("UPDATE auth_kv SET value = json_set(value, '$.access_token', ")
	sb.WriteString(kiroSQLiteQuote(access))
	if r := strings.TrimSpace(refresh); r != "" {
		sb.WriteString(", '$.refresh_token', ")
		sb.WriteString(kiroSQLiteQuote(r))
	}
	if e := strings.TrimSpace(expiresAt); e != "" {
		sb.WriteString(", '$.expires_at', ")
		sb.WriteString(kiroSQLiteQuote(e))
	}
	if p := strings.TrimSpace(profileARN); p != "" {
		sb.WriteString(", '$.profile_arn', ")
		sb.WriteString(kiroSQLiteQuote(p))
	}
	sb.WriteString(") WHERE key = 'kirocli:social:token';\nCOMMIT;\n")
	_, err := kiroRunSQLite(ctx, "-batch", path, sb.String())
	return err
}

func kiroQuerySQLite(ctx context.Context, path, query string) (string, error) {
	out, err := kiroRunSQLite(ctx, "-readonly", "-batch", "-noheader", path, query)
	if err != nil {
		return "", fmt.Errorf("sqlite query on %q: %w", path, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("sqlite query on %q returned no rows", path)
	}
	return value, nil
}

func kiroQuerySQLiteOptional(ctx context.Context, path, query string) (string, bool, error) {
	v, err := kiroQuerySQLite(ctx, path, query)
	if err == nil {
		return v, true, nil
	}
	if strings.Contains(err.Error(), "returned no rows") {
		return "", false, nil
	}
	return "", false, err
}

func kiroRunSQLite(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sqlite3", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return nil, fmt.Errorf("%s", msg)
		}
		return nil, err
	}
	return out, nil
}

func kiroSQLiteQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ── Auth source factory ───────────────────────────────────────────────────────

func newKiroAuthSource(apiKey string) (kiroAuthSource, error) {
	apiKey = strings.TrimSpace(apiKey)
	if !strings.HasPrefix(strings.ToLower(apiKey), "sqlite://") {
		return &staticKiroAuth{token: apiKey}, nil
	}
	// Parse sqlite:// URI
	parsed, err := url.Parse(apiKey)
	if err != nil {
		return nil, fmt.Errorf("parse sqlite URL %q: %w", apiKey, err)
	}
	path := parsed.Path
	if parsed.Host != "" && !strings.HasPrefix(apiKey, "sqlite:///") {
		path = "/" + parsed.Host + parsed.Path
	}
	path, err = url.PathUnescape(path)
	if err != nil {
		return nil, fmt.Errorf("decode sqlite path: %w", err)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("sqlite URL %q does not contain a database path", apiKey)
	}
	return &sqliteKiroAuth{path: path}, nil
}

func kiroDefaultSQLiteKey() string {
	return "sqlite://" + filepath.Join(kiroUserDataDir(), "kiro-cli", "data.sqlite3")
}

func kiroUserDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if d := os.Getenv("LocalAppData"); d != "" {
			return d
		}
	case "darwin":
		if h, _ := os.UserHomeDir(); h != "" {
			return filepath.Join(h, "Library", "Application Support")
		}
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			return d
		}
		if h, _ := os.UserHomeDir(); h != "" {
			return filepath.Join(h, ".local", "share")
		}
	}
	return os.TempDir()
}
