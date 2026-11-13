// This file caches the one auth.test result an agent command needs, so
// a command makes one round trip (its own call) instead of two. See
// "会话缓存" in docs/superpowers/specs/2026-11-12-agent-cli-design.md.
//
// The cache holds only what auth.test returns (slackclient.AgentSession)
// plus the size and modification time of the token file it was made
// for. Never the token or cookie. Any problem with it, in either
// direction, is ignored: the command falls back to Connect.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	slackclient "github.com/gammons/slk/internal/slack"
)

// agentSessionCacheVersion changes when the file's shape does; a file
// with another version is stale.
const agentSessionCacheVersion = 1

type agentSessionCache struct {
	Version       int                      `json:"version"`
	Session       slackclient.AgentSession `json:"session"`
	TokenModNanos int64                    `json:"token_mtime_ns"`
	TokenSize     int64                    `json:"token_size"`
}

type ctxFreshAuthKey struct{}

// withFreshAuth marks ctx so openAgentClient always runs auth.test
// (Connect) and refreshes the cache: for `auth test`, which is the
// health check, and `listen`, which re-checks auth on every reconnect.
func withFreshAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxFreshAuthKey{}, true)
}

func wantsFreshAuth(ctx context.Context) bool {
	v, _ := ctx.Value(ctxFreshAuthKey{}).(bool)
	return v
}

// agentSessionPath is the cache file of a workspace.
func agentSessionPath(teamID string) string {
	return filepath.Join(xdgCache(), "agent-session", teamID+".json")
}

// agentTokenPath is the token file of a workspace (slackclient.TokenStore's layout).
func agentTokenPath(teamID string) string {
	return filepath.Join(xdgData(), "tokens", teamID+".json")
}

// loadAgentSession returns the cached session of teamID if it was made
// for the token file as it is now (same size and modification time).
// The token file is only stat'ed.
func loadAgentSession(teamID string) (slackclient.AgentSession, bool) {
	fi, err := os.Stat(agentTokenPath(teamID))
	if err != nil {
		return slackclient.AgentSession{}, false
	}
	data, err := os.ReadFile(agentSessionPath(teamID))
	if err != nil {
		return slackclient.AgentSession{}, false
	}
	var c agentSessionCache
	if json.Unmarshal(data, &c) != nil || c.Version != agentSessionCacheVersion {
		return slackclient.AgentSession{}, false
	}
	if c.Session.TeamID != teamID || c.Session.UserID == "" ||
		c.TokenModNanos != fi.ModTime().UnixNano() || c.TokenSize != fi.Size() {
		return slackclient.AgentSession{}, false
	}
	return c.Session, true
}

// saveAgentSession records a connected client's session. Failures are
// ignored: the cache only ever makes a command faster.
func saveAgentSession(teamID string, s slackclient.AgentSession) {
	fi, err := os.Stat(agentTokenPath(teamID))
	if err != nil || s.TeamID != teamID {
		return
	}
	data, err := json.Marshal(agentSessionCache{
		Version:       agentSessionCacheVersion,
		Session:       s,
		TokenModNanos: fi.ModTime().UnixNano(),
		TokenSize:     fi.Size(),
	})
	if err != nil {
		return
	}
	dir := filepath.Dir(agentSessionPath(teamID))
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close() // CreateTemp makes the file 0600
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), agentSessionPath(teamID)) != nil {
		os.Remove(tmp.Name())
	}
}

// dropAgentSession deletes the cache file at path (best effort).
func dropAgentSession(path string) {
	if path != "" {
		os.Remove(path)
	}
}
