package slackclient

// TeamURL returns the workspace URL auth.test reported (for example
// "https://acme.slack.com/"), or "" before Connect. The headless agent
// commands in cmd/slk print it (`slk auth test`) and build permalinks
// from it with slackurl.Build, so a send needs no chat.getPermalink.
func (c *Client) TeamURL() string {
	return c.teamURL
}
