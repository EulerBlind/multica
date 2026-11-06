package infoflow

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultAPIBase is the InfoFlow OpenAPI host. The mainland cloud is the only
// region the robot send APIs expose, so unlike Feishu there is no
// per-installation region split.
const defaultAPIBase = "https://api.im.baidu.com/api/v1"

// defaultWSGateway is the InfoFlow WebSocket gateway used for the two-phase
// endpoint fetch.
const defaultWSGateway = "infoflow-open-gateway.baidu.com"

// InfoFlow business error codes the adapter classifies. They arrive inside a
// HTTP 200 envelope as data.errcode, so transport-level retry logic must not
// treat them as success or as a transient network fault.
const (
	// errCodeAPINotAllowed is returned when the application has not been granted
	// the API (observed for POST /app/message/send: 40000 "Api not allowed").
	errCodeAPINotAllowed = 40000
	// errCodeGroupNotFollow is returned when the robot is not a member of the
	// target group (40001 "group not follow").
	errCodeGroupNotFollow = 40001
	// errCodeBodyTooLong / errCodeTextTooLong are the two length rejections the
	// group-send API returns for an oversized message (observed against the
	// live gateway): 40060 "message body length exceed limit" (whole request /
	// body array) and 40061 "message text length exceed limit" (the text run).
	// Both are non-retryable for the SAME payload and must be classified so the
	// outbound path can chunk instead of silently dropping.
	errCodeBodyTooLong = 40060
	errCodeTextTooLong = 40061
	// errCodeQuoteDenied / errCodeQuoteInvalidParam are the getSingleMsg
	// business codes observed on the live gateway (N88). They arrive as
	// data.code, not data.errcode.
	errCodeQuoteDenied       = 720001
	errCodeQuoteInvalidParam = 720027
)

// ErrPermissionNotGranted marks a business error caused by a missing
// application permission (currently the single-chat send API). Callers surface
// it as a configuration/permission problem instead of retrying, because a retry
// cannot succeed until an operator grants the API in the InfoFlow admin
// console.
var ErrPermissionNotGranted = errors.New("infoflow: application permission not granted")

// ErrUnauthorized is returned on HTTP 401 so the outbound sender can drop the
// cached access token and retry once with a fresh one.
var ErrUnauthorized = errors.New("infoflow: unauthorized (access token expired or invalid)")

// ErrMessageTooLong marks a business rejection caused by an oversized message
// body. The sender chunks text well below the observed limit, so this should be
// rare, but a caller that still sees it must not treat the message as
// undeliverable silently.
var ErrMessageTooLong = errors.New("infoflow: message exceeds the platform length limit")

// apiError preserves InfoFlow's machine-readable errcode so callers can
// distinguish an actionable permission problem from a transient failure.
type apiError struct {
	Path       string
	StatusCode int
	Code       int
	Message    string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("infoflow: %s: errcode=%d message=%q", e.Path, e.Code, e.Message)
	}
	return fmt.Sprintf("infoflow: %s: http %d", e.Path, e.StatusCode)
}

// IsPermissionNotGranted reports whether err is (or wraps) a missing-API
// permission failure, so the outbound path can classify it as "permission not
// opened" instead of a crash or a retry storm.
func IsPermissionNotGranted(err error) bool {
	if errors.Is(err, ErrPermissionNotGranted) {
		return true
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		// 40000 covers several distinct business failures. Only the explicit
		// "Api not allowed" text means the application lacks the permission;
		// "lack param" is a request-shape bug and must stay a plain API error.
		return apiErr.Code == errCodeAPINotAllowed && apiErr.Message == "Api not allowed"
	}
	return false
}

// IsMessageTooLong reports whether err is (or wraps) a length rejection, so the
// outbound path can name the cause instead of hiding it behind a generic error.
func IsMessageTooLong(err error) bool {
	if errors.Is(err, ErrMessageTooLong) {
		return true
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Code == errCodeBodyTooLong || apiErr.Code == errCodeTextTooLong
	}
	return false
}

// accessTokenResponse is the success shape of /auth/app_access_token.
type accessTokenResponse struct {
	Code string `json:"code"`
	Data struct {
		AppAccessToken string `json:"app_access_token"`
		Expire         int64  `json:"expire"`
	} `json:"data"`
	Message string `json:"message"`
}

// businessEnvelope is the shared {code, data:{errcode, errmsg}} shape the
// InfoFlow robot APIs return for business-level failures inside HTTP 200. The
// official SDK also emits a flat failure shape {code:"error", errcode, errmsg},
// so the top-level errcode is inspected as well (N79 §3.1).
type businessEnvelope struct {
	Code    string          `json:"code"`
	Errcode int             `json:"errcode"`
	Errmsg  string          `json:"errmsg"`
	Data    json.RawMessage `json:"data"`
}

// Client owns the HTTP transport plus a per-AppKey access-token cache shared by
// the install validator and the outbound sender. It is safe for concurrent use.
type Client struct {
	httpClient *http.Client
	apiBase    string

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// NewClient builds a Client. A nil httpClient uses http.DefaultClient; an empty
// baseURL uses the InfoFlow mainland host.
func NewClient(httpClient *http.Client, baseURL string) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	return &Client{
		httpClient: httpClient,
		apiBase:    strings.TrimRight(baseURL, "/"),
		tokens:     make(map[string]cachedToken),
	}
}

// AccessToken mints (or returns a cached) app_access_token for appKey. The
// request body is {app_key, app_secret: md5hex(appSecret)}; the response
// carries the token and its lifetime in seconds.
func (c *Client) AccessToken(ctx context.Context, appKey, appSecret string) (string, error) {
	if appKey == "" || appSecret == "" {
		return "", errors.New("infoflow: app key and secret are required")
	}
	if token, ok := c.cached(appKey); ok {
		return token, nil
	}
	sum := md5.Sum([]byte(appSecret))
	body, err := json.Marshal(map[string]string{
		"app_key":    appKey,
		"app_secret": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", fmt.Errorf("infoflow: marshal token request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"/auth/app_access_token", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("infoflow: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("infoflow: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("infoflow: read token response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return "", ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &apiError{Path: "/auth/app_access_token", StatusCode: resp.StatusCode}
	}
	var parsed accessTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("infoflow: decode token response: %w", err)
	}
	if parsed.Data.AppAccessToken == "" {
		return "", fmt.Errorf("infoflow: token response missing app_access_token (code=%q message=%q)", parsed.Code, parsed.Message)
	}
	ttl := time.Duration(parsed.Data.Expire) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	// Refresh early so a long send never races the expiry boundary.
	c.store(appKey, parsed.Data.AppAccessToken, time.Now().Add(ttl-time.Minute))
	return parsed.Data.AppAccessToken, nil
}

// Invalidate drops the cached token for appKey after a 401.
func (c *Client) Invalidate(appKey string) {
	c.mu.Lock()
	delete(c.tokens, appKey)
	c.mu.Unlock()
}

func (c *Client) cached(appKey string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.tokens[appKey]
	if !ok || time.Now().After(entry.expiresAt) {
		return "", false
	}
	return entry.token, true
}

func (c *Client) store(appKey, token string, expiresAt time.Time) {
	c.mu.Lock()
	c.tokens[appKey] = cachedToken{token: token, expiresAt: expiresAt}
	c.mu.Unlock()
}

// postJSON posts body to path with the Bearer- auth header and decodes the
// response into out. Business-level errcode values inside HTTP 200 are turned
// into *apiError so callers never mistake a permission failure for success.
func (c *Client) postJSON(ctx context.Context, path, token string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("infoflow: marshal %s request: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("infoflow: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer-"+token)
	return c.doJSON(req, path, out)
}

// getJSON issues an authenticated GET and decodes the business envelope.
func (c *Client) getJSON(ctx context.Context, path, token string, query url.Values, out any) error {
	target := c.apiBase + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("infoflow: build %s request: %w", path, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer-"+token)
	return c.doJSON(req, path, out)
}

// getMessage reads one message back by id. It is the only way to recover the
// full text of a quoted message: the inbound envelope only carries a rendered
// preview, and the returned detail has no body/media URLs.
func (c *Client) getMessage(ctx context.Context, token string, q quoteQuery) (quoteMessageDetail, error) {
	query := url.Values{}
	query.Set("fromId", q.FromID)
	query.Set("receiverId", q.ReceiverID)
	query.Set("receiverType", strconv.Itoa(q.ReceiverType))
	query.Set("msgId", q.MsgID)
	// Keep the raw response so a not-found result can log a redacted skeleton
	// (key names and JSON types only) instead of guessing the live shape.
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/im/message/getSingleMsg", token, query, &raw); err != nil {
		return quoteMessageDetail{}, err
	}
	var resp quoteMessageResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return quoteMessageDetail{}, fmt.Errorf("infoflow: decode getSingleMsg payload: %w", err)
	}
	detail, ok := resp.detail()
	if !ok {
		return quoteMessageDetail{}, &quoteNotFoundError{Skeleton: redactedJSONSkeleton(raw)}
	}
	return detail, nil
}

// quoteMessageResponse covers both getSingleMsg success shapes: the live
// data.data.items[] envelope and the legacy flat data{msgId,content,...}. An
// empty items list or a missing content is a not-found, not a success with
// empty text (DEC16).
type quoteMessageResponse struct {
	Code string `json:"code"`
	Data struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Errcode int             `json:"errcode"`
		Errmsg  string          `json:"errmsg"`
		Data    *struct {
			Items []quoteMessageDetail `json:"items"`
		} `json:"data"`
		quoteMessageDetail
	} `json:"data"`
}

func (r quoteMessageResponse) detail() (quoteMessageDetail, bool) {
	if r.Data.Data != nil {
		if len(r.Data.Data.Items) == 0 {
			return quoteMessageDetail{}, false
		}
		detail := r.Data.Data.Items[0]
		if strings.TrimSpace(detail.Content) == "" {
			return quoteMessageDetail{}, false
		}
		return detail, true
	}
	if strings.TrimSpace(r.Data.Content) == "" {
		return quoteMessageDetail{}, false
	}
	return r.Data.quoteMessageDetail, true
}

// quoteNotFoundError carries a redacted response skeleton for the not-found log.
type quoteNotFoundError struct {
	Skeleton string
}

func (e *quoteNotFoundError) Error() string {
	return "infoflow: getSingleMsg returned no usable message"
}

// redactedJSONSkeleton renders key names and JSON types only; no string value
// (content, URL, user id) can leak through this helper.
func redactedJSONSkeleton(raw []byte) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "invalid_json"
	}
	skeleton := jsonSkeleton(value, 0)
	if len(skeleton) > 600 {
		skeleton = skeleton[:600] + "…"
	}
	return skeleton
}

func jsonSkeleton(value any, depth int) string {
	if depth > 5 {
		return "…"
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+":"+jsonSkeleton(typed[key], depth+1))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		if len(typed) == 0 {
			return "[]"
		}
		return "[" + jsonSkeleton(typed[0], depth+1) + "]"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

// doJSON performs the request and classifies transport, envelope and business
// failures before decoding a success payload into out.
func (c *Client) doJSON(req *http.Request, path string, out any) error {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("infoflow: %s request: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("infoflow: read %s response: %w", path, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{Path: path, StatusCode: resp.StatusCode}
	}

	var envelope businessEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("infoflow: decode %s response: %w", path, err)
	}
	if err := decodeBusinessError(path, envelope); err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("infoflow: decode %s payload: %w", path, err)
		}
	}
	return nil
}

// decodeBusinessError inspects the business-level error the InfoFlow APIs return
// inside HTTP 200: either the top-level {code:"error",errcode,errmsg} shape or
// the nested data.errcode shape (the top-level code then stays "ok").
func decodeBusinessError(path string, envelope businessEnvelope) error {
	if envelope.Errcode != 0 {
		return classifyBusinessError(path, envelope.Errcode, envelope.Errmsg)
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Code), "error") {
		// A failure envelope with no errcode: still not a success payload.
		return &apiError{Path: path, StatusCode: http.StatusOK, Message: "code=error"}
	}
	if len(envelope.Data) == 0 {
		return nil
	}
	var inner struct {
		Errcode int             `json:"errcode"`
		Errmsg  string          `json:"errmsg"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(envelope.Data, &inner); err != nil {
		// Not the {errcode,errmsg} shape: a success payload, not an error.
		return nil
	}
	if inner.Errcode != 0 {
		return classifyBusinessError(path, inner.Errcode, inner.Errmsg)
	}
	// The live gateway puts getSingleMsg business failures in data.code /
	// data.message (720001, 720027, ...) with HTTP 200 and top-level code "ok".
	if code := envelopeCode(inner.Code); code != 0 && code != 200 {
		return classifyBusinessError(path, code, inner.Message)
	}
	return nil
}

// envelopeCode parses data.code whether it arrives as a number or numeric
// string; non-numeric values (e.g. "ok") mean "no business code".
func envelopeCode(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return number
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(text)); err == nil {
			return parsed
		}
	}
	return 0
}

func classifyBusinessError(path string, errcode int, errmsg string) error {
	err := &apiError{Path: path, StatusCode: http.StatusOK, Code: errcode, Message: errmsg}
	if errcode == errCodeAPINotAllowed && errmsg == "Api not allowed" {
		return fmt.Errorf("%w: %v", ErrPermissionNotGranted, err)
	}
	if errcode == errCodeBodyTooLong || errcode == errCodeTextTooLong {
		return fmt.Errorf("%w: %v", ErrMessageTooLong, err)
	}
	return err
}
