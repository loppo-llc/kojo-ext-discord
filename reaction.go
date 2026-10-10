package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// reactionAdd is the MESSAGE_REACTION_ADD payload. It is decoded here
// because discordgo's type lacks message_author_id, which lets most
// reactions be dropped without a REST call.
type reactionAdd struct {
	UserID          string            `json:"user_id"`
	ChannelID       string            `json:"channel_id"`
	MessageID       string            `json:"message_id"`
	GuildID         string            `json:"guild_id"`
	MessageAuthorID string            `json:"message_author_id"`
	Member          *discordgo.Member `json:"member"`
	Emoji           discordgo.Emoji   `json:"emoji"`
}

func (b *Bot) onEvent(_ *discordgo.Session, e *discordgo.Event) {
	if e.Type != "MESSAGE_REACTION_ADD" || !b.cfg.RespondToReactions {
		return
	}
	var r reactionAdd
	if err := json.Unmarshal(e.RawData, &r); err != nil {
		b.log.Debug("unreadable reaction event", "err", err)
		return
	}
	if in := b.acceptReaction(&r); in != nil {
		b.submit(in)
	}
}

// acceptReaction applies the same filters as accept. Only a reaction to
// one of the bot's own messages is addressed to the bot.
func (b *Bot) acceptReaction(r *reactionAdd) *incoming {
	if r.UserID == "" || r.UserID == b.selfID || r.ChannelID == "" || r.MessageID == "" {
		return nil
	}
	if r.Member != nil && r.Member.User != nil && r.Member.User.Bot {
		return nil
	}
	if r.MessageAuthorID != "" && r.MessageAuthorID != b.selfID {
		return nil
	}
	if !b.cfg.AllowedUsers.allows(r.UserID) {
		return nil
	}
	in := &incoming{replyTo: r.ChannelID}
	in.ctx = promptContext{ChannelID: r.ChannelID, MessageID: r.MessageID, UserID: r.UserID, Reaction: true}

	ch, err := b.channel(r.ChannelID)
	if err != nil {
		b.log.Debug("channel lookup failed", "channel", r.ChannelID, "err", err)
		return nil
	}
	if r.GuildID == "" {
		if !b.cfg.RespondToDMs || ch.Type != discordgo.ChannelTypeDM {
			return nil
		}
		in.ctx.IsDM = true
		for _, u := range ch.Recipients {
			if u != nil && u.ID == r.UserID {
				in.ctx.UserName = displayName(u, nil)
			}
		}
	} else {
		if !b.cfg.AllowedGuilds.allows(r.GuildID) || !b.cfg.AllowedChannels.allows(ch.ID, ch.ParentID) {
			return nil
		}
		in.channel = ch
		in.ctx.GuildID = r.GuildID
		in.ctx.ChannelName = ch.Name
		in.ctx.IsThread = ch.IsThread()
		in.ctx.ParentID = ch.ParentID
		if g, err := b.dg.State.Guild(r.GuildID); err == nil {
			in.ctx.GuildName = g.Name
		}
		if r.Member != nil {
			in.ctx.UserName = displayName(r.Member.User, r.Member)
		}
	}

	msg, err := b.dg.ChannelMessage(r.ChannelID, r.MessageID)
	if err != nil {
		b.log.Debug("reacted message lookup failed", "channel", r.ChannelID, "err", err)
		return nil
	}
	if msg.Author == nil || msg.Author.ID != b.selfID {
		return nil
	}
	in.text = reactionText(emojiLabel(&r.Emoji), r.MessageID, msg.Content)
	return in
}

// reactionText describes a reaction to the agent. The quoted text is
// the bot's own message.
func reactionText(emoji, messageID, content string) string {
	text := fmt.Sprintf("(A reaction, not a message.) Reacted with %s to your message (ID %s)", emoji, messageID)
	if excerpt := truncate(strings.Join(strings.Fields(content), " "), 200); excerpt != "" {
		text += fmt.Sprintf(": %q", excerpt)
	}
	return text + ". Answer only if the reaction calls for one; otherwise output exactly `" + noReplyToken + "`."
}

// emojiLabel renders an emoji for the prompt: the character itself, or
// :name: for a server's custom emoji. The name is user-controlled, so
// only what an emoji or an emoji name can consist of is kept.
func emojiLabel(e *discordgo.Emoji) string {
	custom := e.ID != ""
	var sb strings.Builder
	for _, r := range e.Name {
		if sb.Len() >= 48 {
			break
		}
		switch {
		case r >= '0' && r <= '9', r == '#', r == '*':
			sb.WriteRune(r)
		case custom && (r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'):
			sb.WriteRune(r)
		case !custom && r >= 0x80 && r != 0x85 && r != 0x2028 && r != 0x2029 && r != '�':
			sb.WriteRune(r)
		}
	}
	name := sb.String()
	if name == "" {
		return "an emoji"
	}
	if custom {
		return ":" + name + ":"
	}
	return name
}
