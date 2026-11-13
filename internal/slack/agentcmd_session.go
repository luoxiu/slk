package slackclient

import "github.com/slack-go/slack"

// AgentSession is exactly what one auth.test tells the headless agent
// commands (cmd/slk, agentcmd_*.go) about a connected client, and
// nothing secret: no token, no cookie. cmd/slk caches it per workspace
// so a command can skip the auth.test round trip Connect would make.
type AgentSession struct {
	TeamID string `json:"team_id"`
	Team   string `json:"team"`
	UserID string `json:"user_id"`
	User   string `json:"user"`
	URL    string `json:"url"`
}

// AgentSession returns the auth.test result Connect left in c. It is
// only meaningful on a connected client (or one restored by
// RestoreAgentSession).
func (c *Client) AgentSession() AgentSession {
	return AgentSession{
		TeamID: c.teamID,
		Team:   c.teamName,
		UserID: c.userID,
		User:   c.userName,
		URL:    c.teamURL,
	}
}

// RestoreAgentSession puts a fresh NewClient into the state Connect
// leaves it in, from a session AgentSession returned earlier, without a
// network call: the same identity fields, the envelope's team ID, the
// workspace API base URL, and the rebuilt API client. It mirrors
// Connect; keep the two in step.
func (c *Client) RestoreAgentSession(s AgentSession) {
	c.teamID = s.TeamID
	if c.envelope != nil {
		c.envelope.SetTeamID(c.teamID)
	}
	c.userID = s.UserID
	c.userName = s.User
	c.apiBaseURL = deriveAPIBaseURL(s.URL)
	c.teamURL = s.URL
	c.teamName = s.Team

	if c.httpClient != nil {
		c.api = slack.New(
			c.token,
			slack.OptionHTTPClient(c.httpClient),
			slack.OptionAPIURL(c.apiBaseURL),
		)
	}
}
