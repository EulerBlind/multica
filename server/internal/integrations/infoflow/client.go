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
// InfoFlow robot APIs return for business-level failures inside HTTP 200.
type businessEnvelope struct {
	Code string          `json:"code"`
	Data json.RawMessage `json:"data"`
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

// decodeBusinessError inspects the nested data.errcode the InfoFlow robot APIs
// return for business failures (the top-level code stays "ok").
func decodeBusinessError(path string, envelope businessEnvelope) error {
	if len(envelope.Data) == 0 {
		return nil
	}
	var inner struct {
		Errcode int    `json:"errcode"`
		Errmsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(envelope.Data, &inner); err != nil {
		// Not the {errcode,errmsg} shape: a success payload, not an error.
		return nil
	}
	if inner.Errcode == 0 {
		return nil
	}
	err := &apiError{Path: path, StatusCode: http.StatusOK, Code: inner.Errcode, Message: inner.Errmsg}
	if inner.Errcode == errCodeAPINotAllowed && inner.Errmsg == "Api not allowed" {
		return fmt.Errorf("%w: %v", ErrPermissionNotGranted, err)
	}
	return err
}
