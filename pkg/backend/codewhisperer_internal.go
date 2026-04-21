package backend

// Low-level plumbing for the Amazon CodeWhisperer / Kiro backend.
// COVERAGE: Intentionally untested. See codewhisperer.go.
//
//   - Wire types for the GenerateAssistantResponse API
//   - Binary AWS event stream decoder
//   - HTTP API client
//   - SQLite-backed token storage helpers
//   - OIDC token refresh client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"net/http"
	"strings"
)

// ── Wire types ────────────────────────────────────────────────────────────────

const (
	kiroGenerateTarget = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	kiroDefaultOrigin  = "KIRO_CLI"
	kiroTriggerType    = "MANUAL"
	kiroAgentTaskType  = "vibe"
	kiroAmzSDKRequest  = "attempt=1; max=3"

	kiroUserAgent    = "aws-sdk-rust/1.3.14 ua/2.1 api/codewhispererstreaming/0.1.14474 os/linux lang/rust/1.92.0 md/appVersion-1.27.2 app/AmazonQ-For-CLI"
	kiroAmzUserAgent = "aws-sdk-rust/1.3.14 ua/2.1 api/codewhispererstreaming/0.1.14474 os/linux lang/rust/1.92.0 m/F app/AmazonQ-For-CLI"

	kiroOIDCUserAgent    = "aws-sdk-rust/1.3.10 os/linux lang/rust/1.92.0"
	kiroOIDCAmzUserAgent = "aws-sdk-rust/1.3.10 ua/2.1 api/ssooidc/1.92.0 os/linux lang/rust/1.92.0 m/E app/AmazonQ-For-CLI"
)

type kiroRefreshTokenRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	GrantType    string `json:"grantType"`
	RefreshToken string `json:"refreshToken"`
}

type kiroRefreshTokenResponse struct {
	AccessToken string `json:"accessToken"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn   *int   `json:"expiresIn,omitempty"`
}

type kiroGenerateRequest struct {
	ConversationState kiroConversationState `json:"conversationState"`
	ProfileARN        string                `json:"profileArn,omitempty"`
}

type kiroConversationState struct {
	ConversationID      string            `json:"conversationId,omitempty"`
	History             []kiroChatMessage `json:"history,omitempty"`
	CurrentMessage      kiroChatMessage   `json:"currentMessage"`
	ChatTriggerType     string            `json:"chatTriggerType,omitempty"`
	AgentContinuationID string            `json:"agentContinuationId,omitempty"`
	AgentTaskType       string            `json:"agentTaskType,omitempty"`
}

type kiroChatMessage struct {
	UserInputMessage         *kiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *kiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type kiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	UserInputMessageContext *kiroUserInputContext    `json:"userInputMessageContext,omitempty"`
	Origin                  string                   `json:"origin,omitempty"`
	ModelID                 string                   `json:"modelId,omitempty"`
}

type kiroUserInputContext struct {
	ToolResults []kiroToolResult `json:"toolResults,omitempty"`
	Tools       []kiroTool       `json:"tools,omitempty"`
}

type kiroAssistantResponseMessage struct {
	MessageID string       `json:"messageId,omitempty"`
	Content   string       `json:"content"`
	ToolUses  []kiroToolUse `json:"toolUses,omitempty"`
}

type kiroTool struct {
	ToolSpecification *kiroToolSpecification `json:"toolSpecification,omitempty"`
}

type kiroToolSpecification struct {
	InputSchema kiroInputSchema `json:"inputSchema"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
}

type kiroInputSchema struct {
	JSON json.RawMessage `json:"json"`
}

type kiroToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

type kiroToolResult struct {
	ToolUseID string                   `json:"toolUseId"`
	Content   []kiroToolResultContent  `json:"content"`
	Status    string                   `json:"status,omitempty"`
}

type kiroToolResultContent struct {
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}

// stream event types
type kiroMetadataEvent struct {
	ConversationID string `json:"conversationId,omitempty"`
}

type kiroAssistantEvent struct {
	Content string `json:"content"`
}

type kiroToolUseEvent struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     string `json:"input,omitempty"`
	Stop      bool   `json:"stop,omitempty"`
}

type kiroStreamResult struct {
	ToolUses []kiroToolUse
}

// ── Binary AWS event stream decoder ──────────────────────────────────────────

type kiroEventFrame struct {
	Headers     map[string]any
	MessageType string
	EventType   string
	Payload     []byte
}

func kiroReadEventFrame(r io.Reader) (*kiroEventFrame, error) {
	prelude := make([]byte, 12)
	if _, err := io.ReadFull(r, prelude); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read eventstream prelude: %w", err)
	}

	totalLength := binary.BigEndian.Uint32(prelude[0:4])
	headersLength := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	if totalLength < 16 {
		return nil, fmt.Errorf("invalid eventstream frame length %d", totalLength)
	}
	if crc32.ChecksumIEEE(prelude[:8]) != preludeCRC {
		return nil, fmt.Errorf("eventstream prelude crc mismatch")
	}

	remaining := make([]byte, totalLength-12)
	if _, err := io.ReadFull(r, remaining); err != nil {
		return nil, fmt.Errorf("read eventstream remainder: %w", err)
	}

	message := append(prelude, remaining...)
	expectedCRC := binary.BigEndian.Uint32(message[len(message)-4:])
	if crc32.ChecksumIEEE(message[:len(message)-4]) != expectedCRC {
		return nil, fmt.Errorf("eventstream message crc mismatch")
	}

	headersStart := 12
	headersEnd := headersStart + int(headersLength)
	if headersEnd > len(message)-4 {
		return nil, fmt.Errorf("invalid eventstream headers length %d", headersLength)
	}

	headers, err := kiroDecodeHeaders(message[headersStart:headersEnd])
	if err != nil {
		return nil, err
	}

	frame := &kiroEventFrame{
		Headers: headers,
		Payload: append([]byte(nil), message[headersEnd:len(message)-4]...),
	}
	if v, ok := kiroHeaderString(headers, ":message-type"); ok {
		frame.MessageType = v
	}
	if v, ok := kiroHeaderString(headers, ":event-type"); ok {
		frame.EventType = v
	} else if v, ok := kiroHeaderString(headers, ":exception-type"); ok {
		frame.EventType = v
	}
	return frame, nil
}

func kiroDecodeHeaders(data []byte) (map[string]any, error) {
	headers := make(map[string]any)
	for offset := 0; offset < len(data); {
		nameLen := int(data[offset])
		offset++
		if offset+nameLen > len(data) {
			return nil, fmt.Errorf("eventstream header name exceeds buffer")
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(data) {
			return nil, fmt.Errorf("eventstream header missing type")
		}
		val, consumed, err := kiroDecodeHeaderValue(data[offset:])
		if err != nil {
			return nil, err
		}
		offset += consumed
		headers[name] = val
	}
	return headers, nil
}

func kiroDecodeHeaderValue(data []byte) (any, int, error) {
	if len(data) == 0 {
		return nil, 0, fmt.Errorf("empty eventstream header value")
	}
	switch data[0] {
	case 0:
		return true, 1, nil
	case 1:
		return false, 1, nil
	case 2:
		if len(data) < 2 {
			return nil, 0, fmt.Errorf("truncated int8 eventstream header")
		}
		return int8(data[1]), 2, nil
	case 3:
		if len(data) < 3 {
			return nil, 0, fmt.Errorf("truncated int16 eventstream header")
		}
		return int16(binary.BigEndian.Uint16(data[1:3])), 3, nil
	case 4:
		if len(data) < 5 {
			return nil, 0, fmt.Errorf("truncated int32 eventstream header")
		}
		return int32(binary.BigEndian.Uint32(data[1:5])), 5, nil
	case 5, 8:
		if len(data) < 9 {
			return nil, 0, fmt.Errorf("truncated int64 eventstream header")
		}
		return int64(binary.BigEndian.Uint64(data[1:9])), 9, nil
	case 6:
		if len(data) < 3 {
			return nil, 0, fmt.Errorf("truncated bytes eventstream header")
		}
		size := int(binary.BigEndian.Uint16(data[1:3]))
		if len(data) < 3+size {
			return nil, 0, fmt.Errorf("truncated bytes eventstream header payload")
		}
		return base64.StdEncoding.EncodeToString(data[3 : 3+size]), 3 + size, nil
	case 7:
		if len(data) < 3 {
			return nil, 0, fmt.Errorf("truncated string eventstream header")
		}
		size := int(binary.BigEndian.Uint16(data[1:3]))
		if len(data) < 3+size {
			return nil, 0, fmt.Errorf("truncated string eventstream header payload")
		}
		return string(data[3 : 3+size]), 3 + size, nil
	case 9:
		if len(data) < 17 {
			return nil, 0, fmt.Errorf("truncated uuid eventstream header")
		}
		return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
			data[1:5], data[5:7], data[7:9], data[9:11], data[11:17]), 17, nil
	default:
		return nil, 0, fmt.Errorf("unsupported eventstream header type %d", data[0])
	}
}

func kiroHeaderString(headers map[string]any, name string) (string, bool) {
	v, ok := headers[name]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// ── API client ────────────────────────────────────────────────────────────────

type kiroAPIClient struct {
	endpoint     string
	token        string
	httpClient   *http.Client
	extraHeaders map[string]string
}

type kiroAPIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *kiroAPIError) Error() string {
	parts := make([]string, 0, 2)
	if e.Code != "" {
		parts = append(parts, e.Code)
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	return strings.Join(parts, ": ")
}

func (e *kiroAPIError) isUnauthorized() bool {
	return e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden
}

type kiroStreamCallbacks struct {
	OnAssistantDelta func(kiroAssistantEvent)
}

func newKiroAPIClient(endpoint, token string, extraHeaders map[string]string, httpClient *http.Client) *kiroAPIClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &kiroAPIClient{
		endpoint:     strings.TrimRight(endpoint, "/") + "/",
		token:        token,
		httpClient:   httpClient,
		extraHeaders: cloneKiroHeaders(extraHeaders),
	}
}

func (c *kiroAPIClient) Stream(
	ctx context.Context,
	req kiroGenerateRequest,
	callbacks kiroStreamCallbacks,
) (*kiroStreamResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if err := c.applyHeaders(httpReq); err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, kiroDecodeHTTPError(resp)
	}

	result := &kiroStreamResult{}
	accumulators := make(map[string]*kiroToolUseAccumulator)
	order := make([]string, 0)

	for {
		frame, err := kiroReadEventFrame(resp.Body)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		if frame.MessageType == "exception" {
			return nil, kiroDecodeStreamException(frame)
		}
		if frame.MessageType != "" && frame.MessageType != "event" {
			continue
		}

		switch frame.EventType {
		case "assistantResponseEvent":
			var event kiroAssistantEvent
			if err := json.Unmarshal(frame.Payload, &event); err != nil {
				return nil, fmt.Errorf("decode assistant response event: %w", err)
			}
			if callbacks.OnAssistantDelta != nil {
				callbacks.OnAssistantDelta(event)
			}
		case "toolUseEvent":
			var event kiroToolUseEvent
			if err := json.Unmarshal(frame.Payload, &event); err != nil {
				return nil, fmt.Errorf("decode tool use event: %w", err)
			}
			if _, ok := accumulators[event.ToolUseID]; !ok {
				accumulators[event.ToolUseID] = &kiroToolUseAccumulator{
					ToolUseID: event.ToolUseID,
					Name:      event.Name,
				}
				order = append(order, event.ToolUseID)
			}
			acc := accumulators[event.ToolUseID]
			if event.Name != "" {
				acc.Name = event.Name
			}
			acc.Input.WriteString(event.Input)
		}
	}

	toolUses := make([]kiroToolUse, 0, len(order))
	for _, id := range order {
		acc := accumulators[id]
		input := strings.TrimSpace(acc.Input.String())
		if input == "" || !json.Valid([]byte(input)) {
			input = "{}"
		}
		toolUses = append(toolUses, kiroToolUse{
			ToolUseID: acc.ToolUseID,
			Name:      acc.Name,
			Input:     json.RawMessage(input),
		})
	}
	result.ToolUses = toolUses
	return result, nil
}

type kiroToolUseAccumulator struct {
	ToolUseID string
	Name      string
	Input     strings.Builder
}

func (c *kiroAPIClient) applyHeaders(req *http.Request) error {
	invocationID, err := kiroNewUUID()
	if err != nil {
		return fmt.Errorf("generate invocation id: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", kiroGenerateTarget)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", kiroUserAgent)
	req.Header.Set("X-Amz-User-Agent", kiroAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiroAmzSDKRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)
	req.Header.Set("X-Amzn-Codewhisperer-Optout", "false")
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
	return nil
}

func kiroDecodeHTTPError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	_ = json.Unmarshal(body, &envelope)

	e := &kiroAPIError{StatusCode: resp.StatusCode}
	if code := resp.Header.Get("X-Amzn-Errortype"); code != "" {
		e.Code = kiroCleanErrorCode(code)
	}
	if e.Code == "" {
		if v, ok := envelope["code"].(string); ok {
			e.Code = kiroCleanErrorCode(v)
		}
		if e.Code == "" {
			if v, ok := envelope["__type"].(string); ok {
				e.Code = kiroCleanErrorCode(v)
			}
		}
	}
	for _, key := range []string{"message", "Message", "errorMessage"} {
		if v, ok := envelope[key].(string); ok && v != "" {
			e.Message = v
			break
		}
	}
	return e
}

func kiroDecodeStreamException(frame *kiroEventFrame) error {
	var envelope map[string]any
	_ = json.Unmarshal(frame.Payload, &envelope)
	e := &kiroAPIError{Code: kiroCleanErrorCode(frame.EventType)}
	for _, key := range []string{"message", "Message", "errorMessage"} {
		if v, ok := envelope[key].(string); ok && v != "" {
			e.Message = v
			break
		}
	}
	return e
}

func kiroCleanErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Split(value, ":")[0]
	if strings.Contains(value, "#") {
		parts := strings.Split(value, "#")
		value = parts[len(parts)-1]
	}
	return value
}

func kiroDefaultEndpoint(profileARN string) (string, error) {
	parts := strings.Split(profileARN, ":")
	if len(parts) < 6 || parts[0] != "arn" {
		return "", fmt.Errorf("invalid profile ARN %q", profileARN)
	}
	region := parts[3]
	if region == "" {
		return "", fmt.Errorf("profile ARN %q does not contain a region", profileARN)
	}
	return "https://q." + region + ".amazonaws.com/", nil
}

func kiroEndpointForRegion(region string) string {
	return "https://q." + strings.TrimSpace(region) + ".amazonaws.com/"
}

func kiroNewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func cloneKiroHeaders(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}

// ── OIDC token refresh client ─────────────────────────────────────────────────

type kiroOIDCClient struct {
	endpoint   string
	httpClient *http.Client
}

func newKiroOIDCClient(endpoint string, httpClient *http.Client) *kiroOIDCClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &kiroOIDCClient{endpoint: endpoint, httpClient: httpClient}
}

func (c *kiroOIDCClient) RefreshToken(ctx context.Context, req kiroRefreshTokenRequest) (*kiroRefreshTokenResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	invocationID, err := kiroNewUUID()
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "*/*")
	httpReq.Header.Set("Accept-Encoding", "gzip")
	httpReq.Header.Set("User-Agent", kiroOIDCUserAgent)
	httpReq.Header.Set("X-Amz-User-Agent", kiroOIDCAmzUserAgent)
	httpReq.Header.Set("Amz-Sdk-Request", kiroAmzSDKRequest)
	httpReq.Header.Set("Amz-Sdk-Invocation-Id", invocationID)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send refresh request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, kiroDecodeHTTPError(resp)
	}
	var result kiroRefreshTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	if strings.TrimSpace(result.AccessToken) == "" {
		return nil, fmt.Errorf("refresh response did not contain accessToken")
	}
	return &result, nil
}

// ── ListAvailableModels ───────────────────────────────────────────────────────

const (
	kiroListModelsTarget       = "AmazonCodeWhispererService.ListAvailableModels"
	kiroListModelsUserAgent    = "aws-sdk-rust/1.3.14 ua/2.1 api/codewhispererruntime/0.1.14474 os/linux lang/rust/1.92.0 md/appVersion-1.27.2 app/AmazonQ-For-CLI"
	kiroListModelsAmzUserAgent = "aws-sdk-rust/1.3.14 ua/2.1 api/codewhispererruntime/0.1.14474 os/linux lang/rust/1.92.0 m/F,C app/AmazonQ-For-CLI"
)

type kiroListModelsRequest struct {
	Origin     string `json:"origin"`
	ProfileARN string `json:"profileArn,omitempty"`
}

type kiroListModelsResponse struct {
	DefaultModel *kiroAvailableModel  `json:"defaultModel,omitempty"`
	Models       []kiroAvailableModel `json:"models,omitempty"`
}

type kiroAvailableModel struct {
	ModelID     string           `json:"modelId,omitempty"`
	TokenLimits *kiroTokenLimits `json:"tokenLimits,omitempty"`
}

type kiroTokenLimits struct {
	MaxInputTokens  int `json:"maxInputTokens,omitempty"`
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

func (c *kiroAPIClient) ListModels(ctx context.Context, profileARN string) (*kiroListModelsResponse, error) {
	body, err := json.Marshal(kiroListModelsRequest{
		Origin:     kiroDefaultOrigin,
		ProfileARN: profileARN,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	invocationID, _ := kiroNewUUID()
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", kiroListModelsTarget)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", kiroListModelsUserAgent)
	req.Header.Set("X-Amz-User-Agent", kiroListModelsAmzUserAgent)
	req.Header.Set("Amz-Sdk-Request", kiroAmzSDKRequest)
	req.Header.Set("Amz-Sdk-Invocation-Id", invocationID)
	for k, v := range c.extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, kiroDecodeHTTPError(resp)
	}
	var result kiroListModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
