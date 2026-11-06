package infoflow

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/multica-ai/multica/server/internal/integrations/channel"
)

// Labels put on the first line of a quoted block so both the model and a human
// can tell whether the quoted text is the full original or only the preview the
// gateway rendered. QA asserts on these exact strings.
const (
	quoteTagFull      = "[引用·全文]"
	quoteTagPreview   = "[引用·预览]"
	quoteTagTruncated = "[引用·截断]"
	quoteTagOverlong  = "[引用·超长已截断]"
)

// quoteOriginEnv switches the lookup off (off/false/0/disabled/no). Any other
// value — including unset — keeps it on.
const quoteOriginEnv = "MULTICA_INFOFLOW_QUOTE_ORIGIN"

const (
	defaultQuoteTimeout     = 3 * time.Second
	quoteEnrichBudget       = 4 * time.Second
	defaultQuoteConcurrency = 4
	defaultQuoteCacheSize   = 1024

	quoteCacheTTL         = 10 * time.Minute
	quoteNotFoundCacheTTL = 10 * time.Minute
	quoteNegativeCacheTTL = 2 * time.Minute
	quoteDeniedCooldown   = 30 * time.Minute

	quoteTextLimitBytes = 100 << 10
)

// quoteOriginState is the closed set of lookup outcomes. Every failure is
// encoded here instead of being returned as an error, so the inbound path never
// has to choose between losing the quoted text and dropping the main message.
type quoteOriginState string

const (
	quoteStateOK           quoteOriginState = "ok"
	quoteStateDenied       quoteOriginState = "denied"
	quoteStateNotFound     quoteOriginState = "not_found"
	quoteStateInvalidParam quoteOriginState = "invalid_param"
	quoteStateTimeout      quoteOriginState = "timeout"
	quoteStateUnavailable  quoteOriginState = "unavailable"
	quoteStateSkipped      quoteOriginState = "skipped"
)

// quoteQuery is the getSingleMsg request shape. MsgID and ReceiverID stay
// strings: InfoFlow message ids exceed 2^53 and must not round-trip through a
// float64.
type quoteQuery struct {
	FromID       string
	ReceiverID   string
	ReceiverType int
	MsgID        string
}

// quoteMessageDetail mirrors the data object returned by GET
// /im/message/getSingleMsg. It carries plain text only — no structured body and
// no media URL — so quoted images/files always come from the inbound envelope.
type quoteMessageDetail struct {
	MsgID        any    `json:"msgId"`
	FromID       string `json:"fromId"`
	ReceiverID   any    `json:"receiverId"`
	ReceiverType int    `json:"receiverType"`
	MsgType      string `json:"msgType"`
	// Content is flattened from either the legacy string shape or the live
	// {"type":"text","text":"..."} object. UnmarshalJSON below does the work.
	Content    string `json:"-"`
	CreateTime int64  `json:"-"`
}

// UnmarshalJSON accepts both getSingleMsg content shapes and both time field
// names. The live successful item is:
//
//	{"msgId":"...","msgType":"app_msg","sendTime":1791987034577,
//	 "content":{"type":"text","text":"..."}}
//
// while older fixtures and the legacy flat envelope used content:"..." and
// createTime. items:null still yields no detail (see quoteMessageResponse).
func (d *quoteMessageDetail) UnmarshalJSON(data []byte) error {
	var raw struct {
		MsgID        any             `json:"msgId"`
		FromID       string          `json:"fromId"`
		ReceiverID   any             `json:"receiverId"`
		ReceiverType int             `json:"receiverType"`
		MsgType      string          `json:"msgType"`
		Content      json.RawMessage `json:"content"`
		SendTime     int64           `json:"sendTime"`
		CreateTime   int64           `json:"createTime"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	d.MsgID = raw.MsgID
	d.FromID = raw.FromID
	d.ReceiverID = raw.ReceiverID
	d.ReceiverType = raw.ReceiverType
	d.MsgType = raw.MsgType
	d.Content = quoteContentText(raw.Content)
	if raw.SendTime != 0 {
		d.CreateTime = raw.SendTime
	} else {
		d.CreateTime = raw.CreateTime
	}
	return nil
}

// quoteContentText flattens content that is either a string or an object with
// a text field. Anything else is empty, which callers treat as not found.
func quoteContentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	for _, key := range []string{"text", "content", "preview"} {
		if s, ok := object[key].(string); ok {
			return s
		}
	}
	return ""
}

// quoteOrigin is stored under the multica_quote_origin key of the raw payload
// so the platform-specific lookup result stays inside InboundMessage.Raw.
type quoteOrigin struct {
	QueryID    string           `json:"query_id"`
	State      quoteOriginState `json:"state"`
	Source     string           `json:"source"`
	Text       string           `json:"text,omitempty"`
	MsgType    string           `json:"msg_type,omitempty"`
	CreateTime int64            `json:"create_time,omitempty"`
	Error      string           `json:"error,omitempty"`
	ElapsedMS  int64            `json:"elapsed_ms,omitempty"`
	// Matched names the candidate field that produced a successful lookup
	// (sbasemsgid/smsgid2); Attempts is the 1-based candidate attempt for this
	// inbound message (DEC24/DEC25). Neither records the id value.
	Matched  string `json:"matched,omitempty"`
	Attempts int    `json:"attempts"`
}

// clone returns an independent copy. Enrichment attaches an origin to exactly
// one frame's Raw (DEC11), so no caller can observe or mutate another message's
// origin through a shared pointer.
func (o *quoteOrigin) clone() *quoteOrigin {
	if o == nil {
		return nil
	}
	cloned := *o
	return &cloned
}

// quoteEnrichRequest bundles everything one lookup needs. The query is built by
// quoteQueryFor so the parameter mapping (group vs single chat, current-sender
// fromId) lives in one place.
type quoteEnrichRequest struct {
	AppID     string
	AppKey    string
	AppSecret string
	Query     quoteQuery
	// Candidates is the DEC24 sequence sBasemsgId -> sMsgId2. A direct caller
	// that omits it gets the single Query.MsgID candidate.
	Candidates []quoteIDCandidate
}

// quoteEnricher resolves quoted-message full text. It is shared by every
// installation in the process (enforced concurrency, one cache) and is safe for
// concurrent use.
type quoteEnricher struct {
	client  *Client
	logger  *slog.Logger
	timeout time.Duration
	sem     chan struct{}
	cache   *quoteCache

	mu          sync.Mutex
	deniedUntil map[string]time.Time
}

func newQuoteEnricher(client *Client, logger *slog.Logger) *quoteEnricher {
	if logger == nil {
		logger = slog.Default()
	}
	return &quoteEnricher{
		client:      client,
		logger:      logger,
		timeout:     defaultQuoteTimeout,
		sem:         make(chan struct{}, defaultQuoteConcurrency),
		cache:       newQuoteCache(defaultQuoteCacheSize),
		deniedUntil: make(map[string]time.Time),
	}
}

// quoteOriginEnabled reports whether the lookup is switched on. Default on:
// production wiring is shared and must not need an edit to pick the feature up.
func quoteOriginEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(quoteOriginEnv))) {
	case "off", "false", "0", "disabled", "no":
		return false
	default:
		return true
	}
}

// quoteCandidateSequence normalizes the DEC24 order: explicit candidates first,
// otherwise the single query id. It caps work at two candidates and drops
// duplicate ids so a repeated sMsgId2 cannot cause a second identical request.
// quoteMatchedName normalizes the diagnostic field casing for quote_origin's
// matched value, which the contract pins to sbasemsgid/smsgid2.
func quoteMatchedName(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "sbasemsgid":
		return "sbasemsgid"
	case "smsgid2":
		return "smsgid2"
	}
	return strings.ToLower(strings.TrimSpace(field))
}

func quoteCandidateSequence(req quoteEnrichRequest) []quoteIDCandidate {
	out := make([]quoteIDCandidate, 0, 2)
	add := func(candidate quoteIDCandidate) {
		candidate.ID = strings.TrimSpace(candidate.ID)
		if candidate.ID == "" || len(out) >= 2 {
			return
		}
		for _, existing := range out {
			if existing.ID == candidate.ID {
				return
			}
		}
		if candidate.Field == "" {
			candidate.Field = "messageid"
		}
		if candidate.JSONType == "" {
			candidate.JSONType = "string"
		}
		out = append(out, candidate)
	}
	for _, candidate := range req.Candidates {
		add(candidate)
	}
	if len(out) == 0 {
		add(quoteIDCandidate{Field: "messageid", ID: req.Query.MsgID, JSONType: "string"})
	}
	return out
}

// Enrich resolves the quoted text. It never returns an error: a missing id, a
// permission failure, a timeout and a network fault all become a quoteOrigin
// state, and the caller keeps rendering the inbound preview.
//
// DEC24: only not_found advances to the next candidate (sBasemsgId -> sMsgId2),
// at most two attempts. denied/timeout/unavailable stop immediately so a
// suspicious id cannot amplify rate limits. Each candidate has its own cache
// key, so a negative base-id entry cannot suppress the alternate id.
func (e *quoteEnricher) Enrich(ctx context.Context, req quoteEnrichRequest) *quoteOrigin {
	if e == nil || e.client == nil {
		return &quoteOrigin{State: quoteStateSkipped, Error: "enricher not configured"}
	}
	candidates := quoteCandidateSequence(req)
	query := req.Query
	if len(candidates) > 0 {
		query.MsgID = candidates[0].ID
	}
	start := time.Now()
	if len(candidates) == 0 || query.FromID == "" || query.ReceiverID == "" || query.ReceiverType == 0 {
		return &quoteOrigin{State: quoteStateSkipped, QueryID: query.MsgID, Error: "incomplete query"}
	}
	if until, cooling := e.deniedFor(req.AppID, start); cooling {
		return &quoteOrigin{
			QueryID: candidates[0].ID,
			State:   quoteStateDenied,
			Source:  "preview",
			Error:   "api not allowed (cooling down until " + until.Format(time.RFC3339) + ")",
		}
	}

	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	case <-ctx.Done():
		return &quoteOrigin{
			QueryID: candidates[0].ID,
			State:   quoteStateTimeout,
			Source:  "preview",
			Error:   ctx.Err().Error(),
		}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	attempts := 0
	var last *quoteOrigin
	for _, candidate := range candidates {
		attempts++
		key := req.AppID + "|" + candidate.ID
		if cached, ok := e.cache.get(key, start); ok {
			cached.QueryID = candidate.ID
			if cached.State == quoteStateNotFound {
				last = cached
				continue
			}
			if cached.State == quoteStateOK {
				cached.Matched = quoteMatchedName(candidate.Field)
			}
			cached.Attempts = attempts
			return cached
		}

		candidateReq := req
		candidateReq.Query = req.Query
		candidateReq.Query.MsgID = candidate.ID
		origin := e.lookup(lookupCtx, candidateReq)
		origin.QueryID = candidate.ID
		origin.Attempts = attempts
		if origin.State == quoteStateOK {
			origin.Matched = quoteMatchedName(candidate.Field)
		}
		origin.ElapsedMS = time.Since(start).Milliseconds()
		e.remember(key, origin, start)
		if origin.State == quoteStateNotFound {
			last = origin
			continue
		}
		return origin
	}

	if last == nil {
		last = &quoteOrigin{State: quoteStateNotFound, Source: "preview"}
	}
	last.QueryID = candidates[len(candidates)-1].ID
	last.Attempts = attempts
	last.Matched = ""
	last.ElapsedMS = time.Since(start).Milliseconds()
	return last
}

func (e *quoteEnricher) lookup(ctx context.Context, req quoteEnrichRequest) *quoteOrigin {
	token, err := e.client.AccessToken(ctx, req.AppKey, req.AppSecret)
	if err == nil {
		var detail quoteMessageDetail
		detail, err = e.client.getMessage(ctx, token, req.Query)
		if errors.Is(err, ErrUnauthorized) {
			// A stale cached token is recoverable: drop it and retry exactly once.
			e.client.Invalidate(req.AppKey)
			if refreshed, terr := e.client.AccessToken(ctx, req.AppKey, req.AppSecret); terr == nil {
				detail, err = e.client.getMessage(ctx, refreshed, req.Query)
			} else {
				err = terr
			}
		}
		if err == nil {
			return &quoteOrigin{
				State:      quoteStateOK,
				Source:     "api",
				Text:       truncateQuoteText(strings.TrimSpace(detail.Content)),
				MsgType:    strings.TrimSpace(detail.MsgType),
				CreateTime: detail.CreateTime,
			}
		}
		var notFound *quoteNotFoundError
		if errors.As(err, &notFound) && e.logger != nil {
			// Shapes only: no content, URL or user id may enter the log.
			e.logger.Info("infoflow quote origin: not found",
				"app_id", req.AppID,
				"response_skeleton", notFound.Skeleton,
			)
		}
	}

	state, message := classifyQuoteError(err)
	origin := &quoteOrigin{State: state, Source: "preview", Error: message}
	if state == quoteStateDenied {
		if first := e.markDenied(req.AppID, time.Now()); first {
			e.logger.Warn("infoflow quote origin: denied",
				"app_id", req.AppID,
				"errcode", quoteErrorCode(err),
				"error", message,
				"cooldown", quoteDeniedCooldown.String(),
			)
		}
	}
	return origin
}

// remember caches the outcome so one bad message cannot trigger a lookup storm.
func (e *quoteEnricher) remember(key string, origin *quoteOrigin, now time.Time) {
	var ttl time.Duration
	switch origin.State {
	case quoteStateOK:
		ttl = quoteCacheTTL
	case quoteStateNotFound:
		ttl = quoteNotFoundCacheTTL
	case quoteStateDenied:
		ttl = quoteDeniedCooldown
	default:
		ttl = quoteNegativeCacheTTL
	}
	e.cache.set(key, origin, ttl, now)
}

// deniedFor reports the per-application cooldown left after "Api not allowed".
func (e *quoteEnricher) deniedFor(appID string, now time.Time) (time.Time, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.deniedUntil[appID]
	if !ok || now.After(until) {
		if ok {
			delete(e.deniedUntil, appID)
		}
		return time.Time{}, false
	}
	return until, true
}

// markDenied starts the cooldown and reports whether this is the first denial
// for the application (so the operator sees exactly one warning).
func (e *quoteEnricher) markDenied(appID string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.deniedUntil[appID]; ok {
		return false
	}
	e.deniedUntil[appID] = now.Add(quoteDeniedCooldown)
	return true
}

// classifyQuoteError maps a transport/business error onto the origin state.
func classifyQuoteError(err error) (quoteOriginState, string) {
	if err == nil {
		return quoteStateOK, ""
	}
	message := err.Error()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return quoteStateTimeout, message
	}
	var notFound *quoteNotFoundError
	if errors.As(err, &notFound) {
		return quoteStateNotFound, message
	}
	if IsPermissionNotGranted(err) {
		return quoteStateDenied, message
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch {
		case apiErr.StatusCode == 404:
			return quoteStateNotFound, message
		case apiErr.Code == errCodeQuoteDenied:
			return quoteStateDenied, message
		case apiErr.Code == errCodeQuoteInvalidParam:
			return quoteStateInvalidParam, message
		case apiErr.Code == errCodeAPINotAllowed:
			return quoteStateDenied, message
		default:
			// DEC8: 40070 stays unavailable. No documented evidence says it is
			// equivalent to not_found, so it keeps the conservative 2-minute
			// negative cache instead of the 10-minute not-found cache. The same
			// branch covers 40001 group-not-follow and other unknown errcodes.
			return quoteStateUnavailable, message
		}
	}
	return quoteStateUnavailable, message
}

func quoteErrorCode(err error) any {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return nil
}

// truncateQuoteText caps one quoted body at 100KB so a pathological message
// cannot inflate the conversation context.
func truncateQuoteText(text string) string {
	if len(text) <= quoteTextLimitBytes {
		return text
	}
	cut := quoteTextLimitBytes
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + quoteTagOverlong
}

// quoteQueryFor builds the lookup query from one inbound message. msgId prefers
// reply over quote, and the receiver is the group id or the bot's agent id.
// DEC17: fromId is the CURRENT human sender, not the quoted author. The live
// API reads a message only when fromId is a member of the group; the message
// identity is pinned by the quote's own msgId, so a wrong-identity read is
// prevented by requiring both fields.
func quoteQueryFor(msg channel.InboundMessage, payload infoflowRaw, quote *infoflowQuote) (quoteQuery, bool) {
	if quote == nil {
		return quoteQuery{}, false
	}
	query := quoteQuery{
		MsgID:  quoteIDString(quote.MessageID),
		FromID: strings.TrimSpace(msg.Source.SenderID),
	}
	if query.MsgID == "" || query.FromID == "" {
		return quoteQuery{}, false
	}
	switch msg.Source.ChatType {
	case channel.ChatTypeGroup:
		query.ReceiverType = 2
		query.ReceiverID = strings.TrimSpace(msg.Source.ChatID)
	case channel.ChatTypeP2P:
		query.ReceiverType = 7
		query.ReceiverID = strings.TrimSpace(anyString(payload.AppID))
	}
	if query.ReceiverID == "" {
		return quoteQuery{}, false
	}
	return query, true
}

// quoteForPayload returns the quote the enricher should resolve.
func quoteForPayload(payload infoflowRaw) *infoflowQuote {
	if payload.Message != nil {
		if payload.Message.Reply != nil {
			return payload.Message.Reply
		}
		if payload.Message.Quote != nil {
			return payload.Message.Quote
		}
		// N88: the live gateway carries a selected quote as a body element.
		if q := quoteFromBody(payload.Message.Body); q != nil {
			return q
		}
	}
	return payload.Quote
}

// attachQuoteOrigin injects the origin into the message's raw payload and
// re-renders Text/CommandText so flattenQuoted can see it. It reports false when
// the payload no longer normalizes, in which case the caller keeps the original
// message (the main message must never be lost because of enrichment).
func attachQuoteOrigin(msg channel.InboundMessage, origin *quoteOrigin, appID string, self identitySet) (channel.InboundMessage, bool) {
	if origin == nil {
		return msg, false
	}
	// Copy boundary (DEC11): msg.Raw is read-only. decodeInfoflowRaw builds a
	// payload graph owned by this call, the origin is cloned so the cache
	// instance stays private, and normalize* returns a Raw marshaled into a
	// fresh buffer. No shared map/slice/pointer is written here, therefore one
	// frame's origin cannot leak into another frame's Raw.
	payload, err := decodeInfoflowRaw(msg.Raw)
	if err != nil {
		return msg, false
	}
	if payload.AppID == nil || anyString(payload.AppID) == "" {
		payload.AppID = appID
	}
	payload.MulticaQuoteOrigin = origin.clone()
	if isGroupPayload(payload) {
		enriched, _, ok := normalizeGroupPayloadReasonWithIdentity(payload, self)
		return enriched, ok
	}
	enriched, ok := normalizePrivatePayload(payload)
	return enriched, ok
}

// quoteCache is a bounded LRU with per-entry TTL.
type quoteCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

type quoteCacheEntry struct {
	key       string
	origin    *quoteOrigin
	expiresAt time.Time
}

func newQuoteCache(max int) *quoteCache {
	if max <= 0 {
		max = defaultQuoteCacheSize
	}
	return &quoteCache{max: max, ll: list.New(), items: make(map[string]*list.Element, max)}
}

func (c *quoteCache) get(key string, now time.Time) (*quoteOrigin, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*quoteCacheEntry)
	if now.After(entry.expiresAt) {
		c.ll.Remove(elem)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(elem)
	cloned := *entry.origin
	return &cloned, true
}

func (c *quoteCache) set(key string, origin *quoteOrigin, ttl time.Duration, now time.Time) {
	if origin == nil || ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cloned := *origin
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*quoteCacheEntry)
		entry.origin = &cloned
		entry.expiresAt = now.Add(ttl)
		c.ll.MoveToFront(elem)
		return
	}
	entry := &quoteCacheEntry{key: key, origin: &cloned, expiresAt: now.Add(ttl)}
	c.items[key] = c.ll.PushFront(entry)
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		if oldest == nil {
			break
		}
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*quoteCacheEntry).key)
	}
}
