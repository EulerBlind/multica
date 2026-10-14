package infoflow

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Group mention targets in InfoFlow carry the robot's DISPLAY NAME, not its
// imId. The name changes when the robot is renamed, so a group message can look
// unaddressed even though the bot really was @-mentioned. This file resolves the
// installation's own identity inside one group from the robot member list and
// caches it, so the mention check can match on imId/name as well as config.
//
// Everything here is best-effort: a lookup failure or timeout simply leaves the
// identity set as configured, and the mention check stays fail-closed.

const (
	groupIdentityTTL      = 10 * time.Minute
	groupIdentityCacheMax = 64
	groupIdentityTimeout  = 3 * time.Second
	pathGroupMemberList   = "/robot/group/memberList"
)

type groupIdentityEntry struct {
	tokens    []string
	expiresAt time.Time
}

// groupIdentityResolver resolves and caches the bot's own group-scoped identity
// (imId + display name) per (installation app id, group id).
type groupIdentityResolver struct {
	client    *Client
	appKey    string
	appSecret string
	agentID   string
	now       func() time.Time

	mu    sync.Mutex
	cache map[string]groupIdentityEntry
}

func newGroupIdentityResolver(client *Client, appKey, appSecret, agentID string) *groupIdentityResolver {
	return &groupIdentityResolver{
		client:    client,
		appKey:    appKey,
		appSecret: appSecret,
		agentID:   agentID,
		now:       time.Now,
		cache:     make(map[string]groupIdentityEntry),
	}
}

// lookup returns the cached-or-fetched identity tokens for the group. The second
// return value reports whether a live lookup happened (for logging/tests).
func (r *groupIdentityResolver) lookup(ctx context.Context, groupID string) ([]string, bool) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" || r == nil || r.client == nil || r.agentID == "" {
		return nil, false
	}
	key := r.agentID + "|" + groupID

	r.mu.Lock()
	if entry, ok := r.cache[key]; ok && r.now().Before(entry.expiresAt) {
		tokens := append([]string(nil), entry.tokens...)
		r.mu.Unlock()
		return tokens, false
	}
	r.mu.Unlock()

	lookupCtx, cancel := context.WithTimeout(ctx, groupIdentityTimeout)
	defer cancel()
	tokens := r.fetch(lookupCtx, groupID)
	if len(tokens) == 0 {
		return nil, true
	}

	r.mu.Lock()
	r.evictLocked()
	r.cache[key] = groupIdentityEntry{tokens: tokens, expiresAt: r.now().Add(groupIdentityTTL)}
	r.mu.Unlock()
	return append([]string(nil), tokens...), true
}

// evictLocked drops expired entries, then the oldest ones if still over the cap.
func (r *groupIdentityResolver) evictLocked() {
	now := r.now()
	for k, entry := range r.cache {
		if now.After(entry.expiresAt) {
			delete(r.cache, k)
		}
	}
	for len(r.cache) >= groupIdentityCacheMax {
		oldestKey, oldest := "", time.Time{}
		for k, entry := range r.cache {
			if oldest.IsZero() || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = k, entry.expiresAt
			}
		}
		delete(r.cache, oldestKey)
	}
}

// fetch calls /robot/group/memberList and extracts the imId/name of the entry
// whose agentId equals this installation's app id.
func (r *groupIdentityResolver) fetch(ctx context.Context, groupID string) []string {
	token, err := r.client.AccessToken(ctx, r.appKey, r.appSecret)
	if err != nil {
		return nil
	}
	body := map[string]any{"groupId": numericOrString(groupID), "recallType": 0}
	var resp struct {
		Data struct {
			// The live gateway nests the payload one level deeper
			// (data.data.agentInfoList); accept the shallower shape too.
			AgentInfoList []memberListAgent `json:"agentInfoList"`
			Data          struct {
				AgentInfoList []memberListAgent `json:"agentInfoList"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := r.client.postJSON(ctx, pathGroupMemberList, token, body, &resp); err != nil {
		return nil
	}
	agents := resp.Data.AgentInfoList
	if len(agents) == 0 {
		agents = resp.Data.Data.AgentInfoList
	}
	var tokens []string
	for _, agent := range agents {
		if strings.TrimSpace(anyString(agent.AgentID)) != strings.TrimSpace(r.agentID) {
			continue
		}
		for _, candidate := range []any{agent.ImID, agent.IMID, agent.Name} {
			if v := strings.TrimSpace(anyString(candidate)); v != "" {
				tokens = append(tokens, v)
			}
		}
	}
	return tokens
}

// memberListUser is one human entry from userInfoList. The live gateway
// (group 12572761 / 12563594, 2026-10-14) returns only userId, equal to the
// inbound fromuserid. imId is decoded if a future payload adds it, but it is
// not what AT uses.
type memberListUser struct {
	UserID any `json:"userId"`
	ImID   any `json:"imId"`
	Name   any `json:"name"`
}

// atUserIDsForMention picks atuserids for a human @.
//
// The value is the account id (fromuserid / userInfoList.userId). A missing
// member list, a failed lookup, or a list that only carries robot imId all
// keep that account id. imId is never substituted: on the live memberList it
// exists only under agentInfoList.
func atUserIDsForMention(fromUserID string, userInfoList []memberListUser) []string {
	from := strings.TrimSpace(fromUserID)
	if from == "" {
		return nil
	}
	if len(userInfoList) == 0 {
		return []string{from}
	}
	for _, user := range userInfoList {
		if strings.TrimSpace(anyString(user.UserID)) == from {
			return []string{from}
		}
	}
	return []string{from}
}

// memberListAgent is one robot entry in a group member list. The gateway sends
// agentId/imId as JSON numbers, so every field is decoded through anyString.
type memberListAgent struct {
	AgentID any `json:"agentId"`
	ImID    any `json:"imId"`
	IMID    any `json:"imid"`
	Name    any `json:"name"`
}

// numericOrString keeps a numeric group id numeric for the API body, mirroring
// the send path.
func numericOrString(s string) any {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return n
	}
	return s
}
