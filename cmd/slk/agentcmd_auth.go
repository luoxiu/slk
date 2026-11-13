// This file implements `slk auth test`, the smallest complete agent
// command: parse flags, open the client (whose Connect is the one
// auth.test call), print one JSON object. It is the pattern the other
// agentcmd_*.go handlers follow.

package main

import "context"

// agentAuthTestResult is `slk auth test`'s output.
type agentAuthTestResult struct {
	OK     bool   `json:"ok"`
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
	User   string `json:"user"`
	URL    string `json:"url"`
}

// runAgentAuthTest checks the stored session with a single auth.test
// and reports who it is signed in as. No token, or one Slack refuses,
// exits 2.
func runAgentAuthTest(ctx context.Context, env *agentEnv, args []string) error {
	f := newAgentFlags(env, "auth test")
	workspace := f.workspace()
	if _, err := f.parseExact(args); err != nil {
		return err
	}
	client, err := env.openClient(withFreshAuth(ctx), *workspace, "")
	if err != nil {
		return err
	}
	return env.writeJSON(agentAuthTestResult{
		OK:     true,
		TeamID: client.TeamID(),
		Team:   client.TeamName(),
		UserID: client.UserID(),
		User:   client.UserName(),
		URL:    client.TeamURL(),
	})
}
