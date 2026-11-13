package main

import "testing"

// TestRenderAgentContentMatchesAgentSlack runs Slack API message
// objects through slack-go's decoding and renderAgentContent. Each want
// is what agent-slack 0.10.2's renderSlackMessageContent returns for the
// same JSON: its render functions were copied out of the bundle and run
// offline on these fixtures, so a change here is a departure from
// agent-slack, not a style choice.
func TestRenderAgentContentMatchesAgentSlack(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{
			"user rich_text",
			`{"client_msg_id":"6f1c0b2e-0000-4000-8000-000000000001","type":"message","user":"U0AAA1111","ts":"1793932809.791579","team":"T0EXAMPLE1","text":"<@U0BBB2222> see <#C0CCC3333|eng-ops> and <https://example.com/runbook|the runbook> or <https://status.example.com> :tada: :+1::skin-tone-2: Q&amp;A","blocks":[{"type":"rich_text","block_id":"Xy1","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0BBB2222"},{"type":"text","text":" see "},{"type":"channel","channel_id":"C0CCC3333"},{"type":"text","text":" and "},{"type":"link","url":"https://example.com/runbook","text":"the runbook"},{"type":"text","text":" or "},{"type":"link","url":"https://status.example.com"},{"type":"text","text":" "},{"type":"emoji","name":"tada","unicode":"1f389"},{"type":"text","text":" "},{"type":"emoji","name":"+1","unicode":"1f44d-1f3fb","skin_tone":2},{"type":"text","text":" Q&A"}]}]}]}`,
			"@U0BBB2222 see <#C0CCC3333> and [the runbook](https://example.com/runbook) or https://status.example.com 🎉 👍 Q&A",
		},
		{
			"rich_text styles, lists, quote, code",
			`{"type":"message","user":"U0AAA1111","ts":"1793932810.000100","text":"*Plan* for _today_:\n1. ship ~v2~\n2. tell <!channel>\n• <!subteam^S0DDD4444> first\n&gt; line one\n&gt; line two\nmake deploy","blocks":[{"type":"rich_text","block_id":"b2","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"Plan","style":{"bold":true}},{"type":"text","text":" for "},{"type":"text","text":"today","style":{"italic":true,"code":true}},{"type":"text","text":":\n"}]},{"type":"rich_text_list","style":"ordered","indent":0,"border":0,"elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"ship "},{"type":"text","text":"v2","style":{"strike":true}}]},{"type":"rich_text_section","elements":[]},{"type":"rich_text_section","elements":[{"type":"text","text":"tell "},{"type":"broadcast","range":"channel"}]}]},{"type":"rich_text_list","style":"bullet","indent":1,"border":0,"elements":[{"type":"rich_text_section","elements":[{"type":"usergroup","usergroup_id":"S0DDD4444"},{"type":"text","text":" first"}]}]},{"type":"rich_text_quote","elements":[{"type":"text","text":"line one\nline two\n"}]},{"type":"rich_text_preformatted","border":0,"elements":[{"type":"text","text":"make deploy"}]}]}]}`,
			"*Plan* for _`today`_:\n\n\n1. ship ~v2~\n2. tell @channel\n\n- <!subteam^S0DDD4444> first\n\n> line one\n> line two\n\n```make deploy```",
		},
		{
			"text only",
			`{"type":"message","user":"U0AAA1111","ts":"1793932811.000100","text":"<!here> <@U0BBB2222|bob> in <#C0CCC3333|eng-ops>: <https://example.com|docs> &amp; <https://example.com/x> &lt;3 :white_check_mark: :custom-party: <mailto:a@example.com|a@example.com> <!subteam^S0DDD4444>"}`,
			"@here @bob in #eng-ops: [docs](https://example.com) & https://example.com/x <3 ✅ :custom-party: <mailto:a@example.com|a@example.com> <!subteam^S0DDD4444>",
		},
		{
			"bot attachments",
			`{"type":"message","subtype":"bot_message","bot_id":"B0EEE5555","username":"PagerDuty","text":"","ts":"1793932812.000100","attachments":[{"id":1,"color":"d00000","fallback":"Triggered: API latency","pretext":"New incident <!here>","title":"API latency &gt; 2s","title_link":"https://acme.pagerduty.com/incidents/P1","text":"Service *checkout* is degraded","fields":[{"title":"Urgency","value":"High","short":true},{"title":"Assignee","value":"<@U0BBB2222>","short":true},{"title":"","value":"no title","short":false},{"title":"Urgency","value":"High","short":true}],"mrkdwn_in":["text","pretext"]},{"id":2,"color":"36a64f","fallback":"Only a fallback"}]}`,
			"New incident @here\n[API latency > 2s](https://acme.pagerduty.com/incidents/P1)\nService *checkout* is degraded\nUrgency\nHigh\nAssignee\n@U0BBB2222\nno title\n\nOnly a fallback",
		},
		{
			"section blocks",
			`{"type":"message","subtype":"bot_message","bot_id":"B0FFF6666","text":"Deploy finished","ts":"1793932813.000100","blocks":[{"type":"header","block_id":"h","text":{"type":"plain_text","text":"Deploy :rocket:","emoji":true}},{"type":"section","block_id":"s1","text":{"type":"mrkdwn","text":"*api* deployed to <https://prod.example.com|prod> by <@U0AAA1111>"},"fields":[{"type":"mrkdwn","text":"*Version*\nv1.2.3"},{"type":"plain_text","text":"Took 3m","emoji":true}],"accessory":{"type":"button","text":{"type":"plain_text","text":"Open"},"url":"https://ci.example.com/run/42","action_id":"open"}},{"type":"divider","block_id":"d"},{"type":"context","block_id":"c","elements":[{"type":"image","image_url":"https://example.com/i.png","alt_text":"ci"},{"type":"mrkdwn","text":"Triggered by :white_check_mark: CI"}]},{"type":"actions","block_id":"a","elements":[{"type":"button","text":{"type":"plain_text","text":"Rollback"},"url":"https://ci.example.com/rollback/42","action_id":"rb"},{"type":"button","text":{"type":"plain_text","text":"Ack"},"action_id":"ack","value":"x"}]},{"type":"image","block_id":"i","image_url":"https://example.com/graph.png","alt_text":"latency graph"}]}`,
			"*api* deployed to [prod](https://prod.example.com) by @U0AAA1111\n\n*Version*\nv1.2.3\n\nTook 3m\n\nOpen: https://ci.example.com/run/42\n\nTriggered by ✅ CI\n\nRollback: https://ci.example.com/rollback/42\n\nlatency graph: https://example.com/graph.png",
		},
		{
			"blocks with nothing to show fall back to text",
			`{"type":"message","subtype":"bot_message","bot_id":"B0FFF6666","text":"Weekly report &amp; more","ts":"1793932814.000100","blocks":[{"type":"header","block_id":"h","text":{"type":"plain_text","text":"Weekly report","emoji":true}},{"type":"divider","block_id":"d"}]}`,
			"Weekly report & more",
		},
		{
			"forwarded with a comment",
			`{"type":"message","user":"U0AAA1111","text":"FYI","ts":"1793932815.000100","blocks":[{"type":"rich_text","block_id":"f1","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"FYI"}]}]}],"attachments":[{"ts":"1793930000.000100","channel_id":"C0CCC3333","channel_team":"T0EXAMPLE1","is_msg_unfurl":true,"is_share":true,"message_blocks":[{"team":"T0EXAMPLE1","channel":"C0CCC3333","ts":"1793930000.000100","message":{"blocks":[{"type":"rich_text","block_id":"o1","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"Deploy is blocked on "},{"type":"user","user_id":"U0BBB2222"}]}]}]}}],"id":1,"color":"D0D0D0","fallback":"[November 12th, 2026 9:00 AM] carol: Deploy is blocked on <@U0BBB2222>","text":"Deploy is blocked on <@U0BBB2222>","author_id":"U0GGG7777","author_name":"Carol","author_link":"https://acme.slack.com/team/U0GGG7777","author_icon":"https://avatars.slack-edge.com/carol.png","author_subname":"Carol","mrkdwn_in":["text"],"footer":"Posted in #eng-ops","from_url":"https://acme.slack.com/archives/C0CCC3333/p1793930000000100"}]}`,
			"FYI\n\n*Forwarded from [Carol](https://acme.slack.com/team/U0GGG7777) | [original](https://acme.slack.com/archives/C0CCC3333/p1793930000000100)*\n> Deploy is blocked on @U0BBB2222",
		},
		{
			"forwarded without a comment",
			`{"type":"message","user":"U0AAA1111","text":"","ts":"1793932816.000100","attachments":[{"ts":"1793930001.000200","channel_id":"C0CCC3333","is_share":true,"id":1,"fallback":"[November 12th, 2026 9:01 AM] carol: two lines\nsecond","text":"two lines\nsecond","author_name":"Carol","mrkdwn_in":["text"],"from_url":"https://acme.slack.com/archives/C0CCC3333/p1793930001000200?thread_ts=1793930000.000100&cid=C0CCC3333"}]}`,
			"*Forwarded from Carol | [original](https://acme.slack.com/archives/C0CCC3333/p1793930001000200?thread_ts=1793930000.000100&cid=C0CCC3333)*\n> two lines\n> second",
		},
		{
			"link unfurl after the text",
			`{"type":"message","user":"U0AAA1111","text":"see <https://example.com>","ts":"1793932817.000100","blocks":[{"type":"rich_text","block_id":"u1","elements":[{"type":"rich_text_section","elements":[{"type":"text","text":"see "},{"type":"link","url":"https://example.com"}]}]}],"attachments":[{"id":1,"from_url":"https://example.com/","original_url":"https://example.com","service_name":"example.com","title":"Example Domain","title_link":"https://example.com/","text":"This domain is for use in illustrative examples.","fallback":"Example Domain"}]}`,
			"see https://example.com\n\n[Example Domain](https://example.com/)\nThis domain is for use in illustrative examples.",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderAgentContent(decodeMessage(t, tc.raw)); got != tc.want {
				t.Errorf("content\n got  %q\n want %q", got, tc.want)
			}
		})
	}
}

// TestAgentMrkdwnToMarkdown pins the conversion's edges. Every want but
// the last two is agent-slack's output for the same input; those two
// differ on purpose: emoji come from slk's table (Slack's own names),
// which knows skin tones and :robot_face:, where agent-slack's emojilib
// table leaves them as typed.
func TestAgentMrkdwnToMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"&amp;lt;b&amp;gt; stays escaped once", "&lt;b&gt; stays escaped once"},
		{"<!here|here> and <!date^1793932809^{date_short}|Nov 12>", "<!here|here> and <!date^1793932809^{date_short}|Nov 12>"},
		{"<#C0CCC3333> without a name", "<#C0CCC3333> without a name"},
		{"<ftp://files.example.com|files> <mailto:a@example.com>", "<ftp://files.example.com|files> <mailto:a@example.com>"},
		{"at 10:30:45, :not_an_emoji_name: and :custom-party:", "at 10:30:45, :not_an_emoji_name: and :custom-party:"},
		{"<https://example.com/a?b=1&amp;c=2|a &amp; b>", "[a & b](https://example.com/a?b=1&c=2)"},
		{"<@U0BBB2222|bob> <@W0HHH8888>", "@bob @W0HHH8888"},
		{":+1::skin-tone-2:", "👍🏻"},
		{":robot_face:", "🤖"},
	} {
		if got := agentMrkdwnToMarkdown(tc.in); got != tc.want {
			t.Errorf("%q\n got  %q\n want %q", tc.in, got, tc.want)
		}
	}
}

// TestAgentContentMrkdwnKeepsMentions checks the pre-conversion text
// referenced_users reads: mentions from blocks and attachments are still
// <@U…> there.
func TestAgentContentMrkdwnKeepsMentions(t *testing.T) {
	m := decodeMessage(t, `{"type":"message","subtype":"bot_message","bot_id":"B1","text":"","ts":"1.000001",
		"blocks":[{"type":"rich_text","block_id":"b","elements":[{"type":"rich_text_section","elements":[{"type":"user","user_id":"U0AAA1111"}]}]}],
		"attachments":[{"id":1,"fields":[{"title":"Assignee","value":"<@U0BBB2222>","short":true}]}]}`)
	if got, want := agentContentMrkdwn(m), "<@U0AAA1111>\n\nAssignee\n<@U0BBB2222>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
