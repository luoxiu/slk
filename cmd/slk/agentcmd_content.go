// This file renders the `content` of the agent message object the way
// agent-slack's renderSlackMessageContent does, so callers and the
// prompts they feed read the same text after switching from agent-slack
// to slk. It is a port, rule for rule, of agent-slack
// 0.10.2's dist/index.js (MIT): src/slack/render.ts
// (renderSlackMessageContent, extractMrkdwnFromBlocks,
// extractMrkdwnFromAttachments), src/slack/render-rich-text.ts and
// src/slack/mrkdwn.ts (slackMrkdwnToMarkdown). slk's own renderers
// (blockkit.RichTextToMrkdwn, messages.MessageCopyText,
// messages.FlattenMrkdwn) produce different text, so they are not used
// here; the emoji table is slk's (emoji.CodeMap).
//
// What slack-go's types drop, and so what this cannot reproduce: a
// legacy attachment's is_share, is_msg_unfurl, message_blocks, files
// and nested attachments. A shared (forwarded) message is recognized
// by its from_url being a Slack message permalink instead of by
// is_share, and its quoted body is the attachment's text (agent-slack's
// own fallback when message_blocks is absent) rather than the
// original's blocks; files attached to a forward and attachments nested
// in one are not shown.

package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/gammons/slk/internal/emoji"
	"github.com/gammons/slk/internal/slackurl"
	"github.com/slack-go/slack"
)

// renderAgentContent is a message's content: agentContentMrkdwn
// converted to markdown and trimmed. --max-body-chars cuts the result.
func renderAgentContent(m slack.Message) string {
	return jsTrim(agentMrkdwnToMarkdown(agentContentMrkdwn(m)))
}

// agentContentMrkdwn is a message's body as Slack mrkdwn, before the
// markdown conversion: the text of its blocks and of its legacy
// attachments, joined by a blank line, or its text when those have
// none. Mentions are still <@U…> here, which is what referenced_users
// reads.
func agentContentMrkdwn(m slack.Message) string {
	var parts []string
	if b := jsTrim(agentBlocksMrkdwn(m.Blocks.BlockSet)); b != "" {
		parts = append(parts, b)
	}
	if a := jsTrim(agentAttachmentsMrkdwn(m.Attachments)); a != "" {
		parts = append(parts, a)
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n\n")
	}
	return jsTrim(m.Text)
}

// agentBlocksMrkdwn is agent-slack's extractMrkdwnFromBlocks: the text
// of section (text, fields, a button accessory with a URL), actions
// (buttons with a URL), context (text elements), image and rich_text
// blocks, one entry per piece, joined by a blank line. Other block
// types (header, divider, markdown, ...) add nothing. A section's empty
// text or field still adds an (empty) entry, as agent-slack's does.
func agentBlocksMrkdwn(blocks []slack.Block) string {
	var out []string
	for _, b := range blocks {
		switch b := b.(type) {
		case *slack.SectionBlock:
			if isAgentTextObject(b.Text) {
				out = append(out, b.Text.Text)
			}
			for _, f := range b.Fields {
				if isAgentTextObject(f) {
					out = append(out, f.Text)
				}
			}
			if b.Accessory != nil && b.Accessory.ButtonElement != nil {
				if s := agentButtonMrkdwn(b.Accessory.ButtonElement); s != "" {
					out = append(out, s)
				}
			}
		case *slack.ActionBlock:
			if b.Elements == nil {
				continue
			}
			for _, el := range b.Elements.ElementSet {
				if btn, ok := el.(*slack.ButtonBlockElement); ok {
					if s := agentButtonMrkdwn(btn); s != "" {
						out = append(out, s)
					}
				}
			}
		case *slack.ContextBlock:
			for _, el := range b.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok && isAgentTextObject(t) {
					out = append(out, t.Text)
				}
			}
		case *slack.ImageBlock:
			if b.ImageURL != "" {
				out = append(out, agentLabeled(b.AltText, b.ImageURL))
			}
		case *slack.RichTextBlock:
			// slack-go decodes rich_text_input into the same type.
			if b.Type != slack.MBTRichText {
				continue
			}
			if rich := agentRichTextMrkdwn(b); jsTrim(rich) != "" {
				out = append(out, rich)
			}
		}
	}
	return strings.Join(out, "\n\n")
}

func isAgentTextObject(t *slack.TextBlockObject) bool {
	return t != nil && (t.Type == slack.MarkdownType || t.Type == slack.PlainTextType)
}

// agentButtonMrkdwn is a link button as "label: url", "" for a button
// without a URL.
func agentButtonMrkdwn(btn *slack.ButtonBlockElement) string {
	if btn.URL == "" {
		return ""
	}
	label := ""
	if btn.Text != nil {
		label = btn.Text.Text
	}
	return agentLabeled(label, btn.URL)
}

// agentLabeled is "label: url", or url alone when label is empty.
func agentLabeled(label, url string) string {
	if label == "" {
		return url
	}
	return label + ": " + url
}

// ---------------------------------------------------------------------
// rich_text (agent-slack's render-rich-text.ts)
// ---------------------------------------------------------------------

// agentEncodedIDRe is what a user, channel or user group ID must look
// like to be written as a mention; anything else is dropped.
var agentEncodedIDRe = regexp.MustCompile(`^[A-Z0-9]+$`)

// agentRichTextMrkdwn renders a rich_text block as mrkdwn: each
// top-level element that is not blank, joined by a blank line.
func agentRichTextMrkdwn(b *slack.RichTextBlock) string {
	var out []string
	for _, el := range b.Elements {
		if txt := agentRichTextElement(el); jsTrim(txt) != "" {
			out = append(out, txt)
		}
	}
	return strings.Join(out, "\n\n")
}

// agentRichTextElement renders one block-level rich text element. A
// list item is a section; a nested list is a sibling list, and its
// indent is not shown.
func agentRichTextElement(el slack.RichTextElement) string {
	switch el := el.(type) {
	case *slack.RichTextSection:
		return agentRichTextInline(el.Elements)
	case *slack.RichTextPreformatted:
		if text := agentRichTextInline(el.Elements); text != "" {
			return "```" + text + "```"
		}
	case *slack.RichTextQuote:
		if text := jsTrim(agentRichTextInline(el.Elements)); text != "" {
			return agentQuote(text)
		}
	case *slack.RichTextList:
		var items []string
		for _, item := range el.Elements {
			txt := jsTrim(agentRichTextElement(item))
			if txt == "" {
				continue
			}
			prefix := "- "
			if el.Style == slack.RTEListOrdered {
				prefix = strconv.Itoa(len(items)+1) + ". "
			}
			items = append(items, prefix+txt)
		}
		return strings.Join(items, "\n")
	}
	return ""
}

func agentRichTextInline(els []slack.RichTextSectionElement) string {
	var b strings.Builder
	for _, el := range els {
		b.WriteString(agentRichTextInlineElement(el))
	}
	return b.String()
}

// agentRichTextInlineElement renders one inline element. Text keeps its
// characters as typed (no entity escaping) with code, bold, italic and
// strike marks applied in that order; mentions become Slack's wire
// form; date, team, color and link-mention chips render as nothing.
func agentRichTextInlineElement(el slack.RichTextSectionElement) string {
	switch el := el.(type) {
	case *slack.RichTextSectionTextElement:
		text := el.Text
		if s := el.Style; s != nil {
			if s.Code {
				text = "`" + text + "`"
			}
			if s.Bold {
				text = "*" + text + "*"
			}
			if s.Italic {
				text = "_" + text + "_"
			}
			if s.Strike {
				text = "~" + text + "~"
			}
		}
		return text
	case *slack.RichTextSectionLinkElement:
		switch {
		case el.URL == "":
			return el.Text
		case el.Text != "":
			return "<" + el.URL + "|" + el.Text + ">"
		}
		return el.URL
	case *slack.RichTextSectionEmojiElement:
		if el.Name != "" {
			return ":" + el.Name + ":"
		}
	case *slack.RichTextSectionUserElement:
		if agentEncodedIDRe.MatchString(el.UserID) {
			return "<@" + el.UserID + ">"
		}
	case *slack.RichTextSectionChannelElement:
		if agentEncodedIDRe.MatchString(el.ChannelID) {
			return "<#" + el.ChannelID + ">"
		}
	case *slack.RichTextSectionUserGroupElement:
		if agentEncodedIDRe.MatchString(el.UsergroupID) {
			return "<!subteam^" + el.UsergroupID + ">"
		}
	case *slack.RichTextSectionBroadcastElement:
		switch el.Range {
		case "here", "channel", "everyone":
			return "<!" + el.Range + ">"
		}
	}
	return ""
}

// ---------------------------------------------------------------------
// legacy attachments
// ---------------------------------------------------------------------

// agentAttachmentsMrkdwn is agent-slack's extractMrkdwnFromAttachments
// over what slack-go keeps of an attachment (see the file comment). A
// shared message is a header and its quoted text; any other attachment
// is its blocks, pretext, title (linked), text and fields, one per line
// with repeats dropped, or its fallback when it has none of those.
// Attachments are joined by a blank line, repeats dropped.
func agentAttachmentsMrkdwn(atts []slack.Attachment) string {
	var parts []string
	for _, a := range atts {
		if _, shared := slackurl.Parse(a.FromURL); shared {
			chunk := []string{agentForwardHeader(a)}
			if body := jsTrim(a.Text); body != "" {
				chunk = append(chunk, agentQuote(body))
			}
			parts = append(parts, strings.Join(chunk, "\n"))
			continue
		}
		var chunk []string
		if blocks := agentBlocksMrkdwn(a.Blocks.BlockSet); jsTrim(blocks) != "" {
			chunk = append(chunk, blocks)
		}
		if a.Pretext != "" {
			chunk = append(chunk, a.Pretext)
		}
		switch {
		case a.TitleLink != "" && a.Title != "":
			chunk = append(chunk, "<"+a.TitleLink+"|"+a.Title+">")
		case a.Title != "":
			chunk = append(chunk, a.Title)
		case a.TitleLink != "":
			chunk = append(chunk, a.TitleLink)
		}
		if a.Text != "" {
			chunk = append(chunk, a.Text)
		}
		for _, f := range a.Fields {
			switch {
			case f.Title != "" && f.Value != "":
				chunk = append(chunk, f.Title+"\n"+f.Value)
			case f.Title != "":
				chunk = append(chunk, f.Title)
			case f.Value != "":
				chunk = append(chunk, f.Value)
			}
		}
		if len(chunk) == 0 && a.Fallback != "" {
			chunk = append(chunk, a.Fallback)
		}
		if len(chunk) > 0 {
			parts = append(parts, strings.Join(agentUniqueTexts(chunk), "\n"))
		}
	}
	return strings.Join(agentUniqueTexts(parts), "\n\n")
}

// agentForwardHeader is the line above a shared message's quoted text:
// who wrote it (linked to their profile) and a link to the original.
func agentForwardHeader(a slack.Attachment) string {
	author := a.AuthorName
	if a.AuthorName != "" && a.AuthorLink != "" {
		author = "<" + a.AuthorLink + "|" + a.AuthorName + ">"
	}
	source := ""
	if a.FromURL != "" {
		source = "<" + a.FromURL + "|original>"
	}
	switch {
	case author != "" && source != "":
		return "*Forwarded from " + author + " | " + source + "*"
	case author != "":
		return "*Forwarded from " + author + "*"
	case source != "":
		return "*Forwarded message | " + source + "*"
	}
	return "*Forwarded message*"
}

// agentQuote prefixes every line of text with "> ".
func agentQuote(text string) string {
	return "> " + strings.ReplaceAll(text, "\n", "\n> ")
}

// agentUniqueTexts trims each value and keeps the first of each, empty
// ones dropped.
func agentUniqueTexts(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		t := jsTrim(v)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// ---------------------------------------------------------------------
// mrkdwn to markdown (agent-slack's slackMrkdwnToMarkdown)
// ---------------------------------------------------------------------

var (
	agentLabeledLinkRe = regexp.MustCompile(`<((https?://)[^>|]+)\|([^>]+)>`)
	agentBareLinkRe    = regexp.MustCompile(`<((https?://)[^>]+)>`)
	agentChannelRefRe  = regexp.MustCompile(`<#[A-Z0-9]+\|([^>]+)>`)
	agentNamedUserRe   = regexp.MustCompile(`<@([A-Z0-9]+)\|([^>]+)>`)
	agentUserRefRe     = regexp.MustCompile(`<@([A-Z0-9]+)>`)
	agentSpecialRefRe  = regexp.MustCompile(`<!([a-zA-Z]+)>`)
	// agentShortcodeRe is node-emoji's emojify pattern, /:[\w\-+]+:/g,
	// which is also internal/emoji's shortcodeRe (unexported there).
	agentShortcodeRe = regexp.MustCompile(`:[A-Za-z0-9_+\-]+:`)
)

// agentMrkdwnToMarkdown converts Slack mrkdwn to the markdown
// agent-slack prints: <url|label> is [label](url) and <url> is url
// (http and https only), <#C…|name> is #name (an unnamed <#C…> stays),
// <@U…|name> is @name, <@U…> is @U…, <!here> and the like are @here,
// &lt; &gt; &amp; are decoded, and a :shortcode: of a standard emoji is
// its glyph (custom and unknown ones stay as typed). <!subteam^…> and
// <!date^…> are left as they are.
func agentMrkdwnToMarkdown(s string) string {
	s = agentLabeledLinkRe.ReplaceAllString(s, "[${3}](${1})")
	s = agentBareLinkRe.ReplaceAllString(s, "${1}")
	s = agentChannelRefRe.ReplaceAllString(s, "#${1}")
	s = agentNamedUserRe.ReplaceAllString(s, "@${2}")
	s = agentUserRefRe.ReplaceAllString(s, "@${1}")
	s = agentSpecialRefRe.ReplaceAllString(s, "@${1}")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&amp;", "&")
	codes := emoji.CodeMap()
	return agentShortcodeRe.ReplaceAllStringFunc(s, func(code string) string {
		if glyph, ok := codes[code]; ok {
			return glyph
		}
		return code
	})
}

// jsTrim is JavaScript's String.prototype.trim, which agent-slack
// trims with: unlike strings.TrimSpace it strips U+FEFF and keeps
// U+0085.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == '\uFEFF' || (r != '\u0085' && unicode.IsSpace(r))
	})
}
