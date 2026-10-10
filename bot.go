package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	typingInterval     = 8 * time.Second
	maxAttachments     = 10
	maxAttachmentBytes = 25 << 20
	maxQueuedPerChat   = 5
	stopCommand        = "!stop"
	busyRetries        = 5
)

// Bot bridges one Discord bot account to one kojo agent.
type Bot struct {
	env     Env
	cfg     Config
	kojo    *KojoClient
	dg      *discordgo.Session
	log     *slog.Logger
	threads *threadStore
	http    *http.Client

	selfID string

	mu       sync.Mutex
	sessions map[string]*chatSession
}

// chatSession serialises the turns of one conversation. kojo refuses a
// second concurrent turn for a session key, so a message that arrives
// mid-turn is steered into the running turn or queued behind it.
type chatSession struct {
	cancel context.CancelFunc
	queue  []*incoming
	// stopping is set by !stop until the cancelled turn has ended.
	// Messages that arrive meanwhile queue instead of being steered
	// into a turn that is on its way out and would swallow them.
	stopping bool
}

// incoming is a Discord message that passed the filters, with the
// conversation it belongs to already resolved.
type incoming struct {
	msg       *discordgo.Message
	text      string
	channel   *discordgo.Channel // nil for a DM
	replyTo   string             // channel the answer goes to
	reference bool               // answer as a Discord reply to msg
	ctx       promptContext
}

func newBot(env Env, dg *discordgo.Session, log *slog.Logger) *Bot {
	return &Bot{
		env:      env,
		cfg:      env.Config,
		kojo:     newKojoClient(env),
		dg:       dg,
		log:      log,
		threads:  newThreadStore(env.DataDir),
		http:     &http.Client{Timeout: 2 * time.Minute},
		sessions: map[string]*chatSession{},
	}
}

func sessionKey(channelID string) string { return "ch-" + channelID }

// channel resolves a channel from the gateway cache, falling back to
// REST for one the cache has not seen (an archived thread, usually).
func (b *Bot) channel(id string) (*discordgo.Channel, error) {
	if ch, err := b.dg.State.Channel(id); err == nil {
		return ch, nil
	}
	ch, err := b.dg.Channel(id)
	if err != nil {
		return nil, err
	}
	_ = b.dg.State.ChannelAdd(ch)
	return ch, nil
}

func mentionsUser(m *discordgo.Message, userID string) bool {
	for _, u := range m.Mentions {
		if u != nil && u.ID == userID {
			return true
		}
	}
	return false
}

// cleanContent drops the bot's own mention and turns the others into
// readable @names, so the agent does not see raw <@123> tokens.
func cleanContent(m *discordgo.Message, selfID string) string {
	text := m.Content
	for _, form := range []string{"<@" + selfID + ">", "<@!" + selfID + ">"} {
		text = strings.ReplaceAll(text, form, "")
	}
	for _, u := range m.Mentions {
		if u == nil || u.ID == selfID {
			continue
		}
		name := "@" + sanitizeName(displayName(u, nil))
		text = strings.ReplaceAll(text, "<@"+u.ID+">", name)
		text = strings.ReplaceAll(text, "<@!"+u.ID+">", name)
	}
	return strings.TrimSpace(text)
}

func displayName(u *discordgo.User, member *discordgo.Member) string {
	if member != nil && member.Nick != "" {
		return member.Nick
	}
	if u == nil {
		return ""
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

func (b *Bot) onMessageCreate(_ *discordgo.Session, mc *discordgo.MessageCreate) {
	m := mc.Message
	if m == nil || m.Author == nil {
		return
	}
	if m.Author.ID == b.selfID {
		// The bot's own post — from a reply, or from the agent calling
		// the MCP tools — marks a thread as one it takes part in.
		if m.GuildID != "" {
			if ch, err := b.channel(m.ChannelID); err == nil && ch.IsThread() {
				b.threads.add(ch.ID)
			}
		}
		return
	}
	if m.Author.Bot || m.Author.System || m.WebhookID != "" {
		return
	}
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return
	}
	in := b.accept(m)
	if in == nil {
		return
	}
	b.dispatch(in)
}

// accept applies the operator's filters and decides whether the bot
// was addressed. It returns nil for a message to ignore.
func (b *Bot) accept(m *discordgo.Message) *incoming {
	if !b.cfg.AllowedUsers.allows(m.Author.ID) {
		return nil
	}
	in := &incoming{msg: m, replyTo: m.ChannelID, reference: true}
	in.ctx = promptContext{
		ChannelID: m.ChannelID,
		MessageID: m.ID,
		UserID:    m.Author.ID,
		UserName:  displayName(m.Author, m.Member),
	}
	mentioned := mentionsUser(m, b.selfID) ||
		(m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == b.selfID)

	if m.GuildID == "" {
		if !b.cfg.RespondToDMs {
			return nil
		}
		ch, err := b.channel(m.ChannelID)
		if err != nil || ch.Type != discordgo.ChannelTypeDM {
			return nil
		}
		in.ctx.IsDM = true
		in.reference = false
	} else {
		if !b.cfg.AllowedGuilds.allows(m.GuildID) {
			return nil
		}
		ch, err := b.channel(m.ChannelID)
		if err != nil {
			b.log.Warn("channel lookup failed", "channel", m.ChannelID, "err", err)
			return nil
		}
		if !b.cfg.AllowedChannels.allows(ch.ID, ch.ParentID) {
			return nil
		}
		inTrackedThread := ch.IsThread() && b.cfg.RespondInThreads && b.threads.has(ch.ID)
		if !(mentioned && b.cfg.RespondToMentions) && !inTrackedThread {
			return nil
		}
		in.channel = ch
		in.ctx.GuildID = m.GuildID
		in.ctx.ChannelName = ch.Name
		in.ctx.IsThread = ch.IsThread()
		in.ctx.ParentID = ch.ParentID
		if g, err := b.dg.State.Guild(m.GuildID); err == nil {
			in.ctx.GuildName = g.Name
		}
	}

	in.text = cleanContent(m, b.selfID)
	if in.text == "" && len(m.Attachments) == 0 {
		if !mentionsUser(m, b.selfID) && !in.ctx.IsDM {
			// Without the Message Content intent Discord blanks the
			// text of anything that does not mention the bot.
			b.log.Warn("message has no readable content; enable the Message Content intent to follow threads and un-pinged replies",
				"channel", m.ChannelID)
			return nil
		}
		in.text = "(The user mentioned you without writing anything.)"
	}
	if in.text == "" {
		// kojo requires a message; the files alone are the message.
		in.text = "(The user sent the attached file(s) without writing anything.)"
	}
	return in
}

// dispatch routes an accepted message: start a turn, steer the one
// already running, or queue behind it.
func (b *Bot) dispatch(in *incoming) {
	if strings.EqualFold(in.text, stopCommand) {
		b.mu.Lock()
		sess := b.sessions[sessionKey(in.replyTo)]
		if sess != nil {
			if sess.cancel != nil {
				sess.cancel()
			}
			sess.queue = nil
			sess.stopping = true
		}
		b.mu.Unlock()
		if sess != nil {
			b.react(in.msg, "🛑")
		}
		return
	}
	b.maybeStartThread(in)
	key := sessionKey(in.replyTo)

	b.mu.Lock()
	sess := b.sessions[key]
	if sess == nil {
		sess, ctx := b.newSessionLocked(key)
		b.mu.Unlock()
		b.runSession(ctx, key, sess, in)
		return
	}
	if sess.stopping {
		if len(sess.queue) >= maxQueuedPerChat {
			b.mu.Unlock()
			b.react(in.msg, "⏳")
			return
		}
		sess.queue = append(sess.queue, in)
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	steerCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := b.kojo.Steer(steerCtx, key, b.steerText(in))
	cancel()
	if err == nil {
		b.react(in.msg, "👀")
		return
	}
	if apiErrorCode(err) == "steer_uncertain" {
		// kojo could not confirm delivery but the turn may have it.
		// Running it again as a new turn could do the work twice.
		b.log.Warn("steer delivery unconfirmed; not resending", "err", err)
		b.react(in.msg, "👀")
		return
	}
	if code := apiErrorCode(err); code != "not_busy" && code != "steer_unsupported" {
		b.log.Warn("steer failed; queueing instead", "err", err)
	}

	b.mu.Lock()
	if cur := b.sessions[key]; cur != nil {
		if len(cur.queue) >= maxQueuedPerChat {
			b.mu.Unlock()
			b.react(in.msg, "⏳")
			return
		}
		cur.queue = append(cur.queue, in)
		b.mu.Unlock()
		return
	}
	// The turn finished while the steer was in flight.
	sess, ctx := b.newSessionLocked(key)
	b.mu.Unlock()
	b.runSession(ctx, key, sess, in)
}

// newSessionLocked registers a session that is already cancellable, so
// a !stop arriving before its first turn starts still stops it.
// Caller holds b.mu.
func (b *Bot) newSessionLocked(key string) (*chatSession, context.Context) {
	ctx, cancel := context.WithCancel(context.Background())
	sess := &chatSession{cancel: cancel}
	b.sessions[key] = sess
	return sess, ctx
}

// steerText labels a mid-turn message with its author: in a shared
// channel it may not be the person the turn is answering.
func (b *Bot) steerText(in *incoming) string {
	text := in.text
	if n := len(in.msg.Attachments); n > 0 {
		text += fmt.Sprintf("\n(%d attachment(s) were sent with this message; ask for them again if you need them.)", n)
	}
	if in.ctx.IsDM {
		return text
	}
	return fmt.Sprintf("[%s (user ID %s)] %s", sanitizeName(in.ctx.UserName), in.ctx.UserID, text)
}

// maybeStartThread moves the conversation into a new thread on the
// triggering message when the operator asked for that.
func (b *Bot) maybeStartThread(in *incoming) {
	if !b.cfg.ReplyInThread || in.channel == nil || in.channel.IsThread() {
		return
	}
	if in.channel.Type != discordgo.ChannelTypeGuildText && in.channel.Type != discordgo.ChannelTypeGuildNews {
		return
	}
	name := strings.Join(strings.Fields(in.text), " ")
	if cut := cutAt(name, 80); cut < len(name) {
		name = name[:cut] + "…"
	}
	if name == "" {
		name = "chat"
	}
	th, err := b.dg.MessageThreadStartComplex(in.msg.ChannelID, in.msg.ID, &discordgo.ThreadStart{
		Name:                name,
		AutoArchiveDuration: 1440,
	})
	if err != nil {
		b.log.Warn("could not start a thread; replying in the channel", "channel", in.msg.ChannelID, "err", err)
		return
	}
	b.threads.add(th.ID)
	in.replyTo = th.ID
	in.reference = false
	in.ctx.IsThread = true
	in.ctx.ParentID = in.msg.ChannelID
	in.ctx.ChannelID = th.ID
	in.ctx.ChannelName = th.Name
}

// runSession runs in's turn and then whatever queued up behind it.
func (b *Bot) runSession(ctx context.Context, key string, sess *chatSession, in *incoming) {
	for in != nil {
		b.runTurn(ctx, key, in)

		// The next turn's cancel is installed in the same critical
		// section that takes it off the queue, so a !stop always finds
		// either the message still queued or a turn it can cancel.
		b.mu.Lock()
		sess.cancel()
		sess.stopping = false
		if len(sess.queue) == 0 {
			sess.cancel = nil
			delete(b.sessions, key)
			in = nil
		} else {
			in = sess.queue[0]
			sess.queue = sess.queue[1:]
			ctx, sess.cancel = context.WithCancel(context.Background())
		}
		b.mu.Unlock()
	}
}

func (b *Bot) runTurn(ctx context.Context, key string, in *incoming) {
	stopTyping := b.startTyping(ctx, in.replyTo)
	defer stopTyping()

	attachments, skipped := b.downloadAttachments(ctx, in.msg)
	message := in.text
	if skipped > 0 {
		message += fmt.Sprintf("\n(%d attachment(s) could not be downloaded and are not included.)", skipped)
	}
	req := ChatRequest{
		SessionKey:        key,
		Message:           message,
		SystemPromptExtra: buildSystemPromptExtra(in.ctx),
		History:           b.history(in),
		HistorySelfUserID: b.selfID,
		Attachments:       attachments,
	}

	var (
		resp     responseBuilder
		terminal string
		failure  string
		stopped  bool
	)
	onEvent := func(evt ChatEvent) {
		// A subagent's text belongs to its Task tool, not to the reply.
		if evt.ParentToolUseID != "" {
			return
		}
		switch evt.Type {
		case "text":
			resp.addText(evt.Delta)
		case "tool_use":
			resp.toolBoundary()
		case "error":
			if failure == "" {
				failure = evt.ErrorMessage
			}
		case "done":
			if evt.Message != nil {
				terminal = evt.Message.Content
			}
			if evt.ErrorMessage != "" && failure == "" {
				failure = evt.ErrorMessage
			}
		}
	}

	var err error
	for attempt := 0; ; attempt++ {
		err = b.kojo.Chat(ctx, req, onEvent)
		// kojo still holds the session for a moment after the previous
		// turn's stream closes; "busy" right after one is that window.
		if apiErrorCode(err) != "busy" || attempt >= busyRetries {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	stopTyping()
	if ctx.Err() != nil {
		stopped = true
	}

	text := resp.final(terminal)
	switch {
	case stopped:
		// The operator's !stop already got its reaction; post what
		// was written so far, if anything.
		if strings.TrimSpace(text) != "" && !isNoReply(text) {
			b.post(in, text)
		}
		return
	case err != nil:
		b.log.Warn("turn failed", "session", key, "err", err)
		if strings.TrimSpace(text) != "" && !isNoReply(text) {
			b.post(in, text)
		}
		b.post(in, "⚠️ "+userFacingError(err))
		return
	case failure != "":
		b.log.Warn("agent reported an error", "session", key, "err", failure)
		if strings.TrimSpace(text) != "" && !isNoReply(text) {
			b.post(in, text)
		}
		b.post(in, "⚠️ "+truncate(failure, 500))
		return
	}
	if strings.TrimSpace(text) == "" || isNoReply(text) {
		return
	}
	b.post(in, text)
}

func userFacingError(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "busy":
			return "The agent is still busy with the previous message in this conversation. Try again in a moment."
		case "not_found":
			return "The kojo agent this bot is bound to no longer exists."
		case "unavailable", "chat_rejected":
			return "The agent cannot take messages right now: " + truncate(ae.Message, 300)
		}
		return "kojo refused the message: " + truncate(ae.Message, 300)
	}
	return "The connection to kojo was lost before the reply was complete."
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if cut := cutAt(s, max); cut < len(s) {
		return s[:cut] + "…"
	}
	return s
}

// startTyping keeps the typing indicator alive until the returned
// function is called. Discord clears it after about ten seconds.
func (b *Bot) startTyping(ctx context.Context, channelID string) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(typingInterval)
		defer t.Stop()
		for {
			_ = b.dg.ChannelTyping(channelID, discordgo.WithContext(ctx))
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return cancel
}

// post sends text to the conversation, split to Discord's limit. Only
// user mentions ping: an agent must not be able to @everyone.
func (b *Bot) post(in *incoming, text string) {
	for i, chunk := range splitMessage(text, discordMessageLimit) {
		send := &discordgo.MessageSend{
			Content:         chunk,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}},
		}
		if i == 0 && in.reference {
			lenient := false
			send.Reference = &discordgo.MessageReference{
				MessageID:       in.msg.ID,
				ChannelID:       in.msg.ChannelID,
				GuildID:         in.msg.GuildID,
				FailIfNotExists: &lenient,
			}
		}
		if _, err := b.dg.ChannelMessageSendComplex(in.replyTo, send); err != nil {
			b.log.Error("posting to Discord failed", "channel", in.replyTo, "err", err)
			return
		}
	}
	if in.ctx.IsThread {
		b.threads.add(in.replyTo)
	}
}

func (b *Bot) react(m *discordgo.Message, emoji string) {
	if err := b.dg.MessageReactionAdd(m.ChannelID, m.ID, emoji); err != nil {
		b.log.Debug("reaction failed", "err", err)
	}
}

// history returns the messages that preceded the current one, oldest
// first, as context for a session kojo has not seen before.
func (b *Bot) history(in *incoming) []HistoryMessage {
	if b.cfg.HistoryLimit <= 0 {
		return nil
	}
	msgs, err := b.dg.ChannelMessages(in.msg.ChannelID, b.cfg.HistoryLimit, in.msg.ID, "", "")
	if err != nil {
		b.log.Debug("history fetch failed", "channel", in.msg.ChannelID, "err", err)
		return nil
	}
	return toHistory(b.trusted(msgs), in.ctx.ParentID)
}

// trusted drops messages written by people the bot would not answer.
// Without this an allowlisted user's question would carry everyone
// else's earlier messages to the agent as context — a way around the
// allowlist for anyone who can post in the channel.
func (b *Bot) trusted(msgs []*discordgo.Message) []*discordgo.Message {
	if len(b.cfg.AllowedUsers) == 0 {
		return msgs
	}
	out := make([]*discordgo.Message, 0, len(msgs))
	for _, m := range msgs {
		if m == nil || m.Author == nil {
			continue
		}
		if m.Author.ID == b.selfID || b.cfg.AllowedUsers.allows(m.Author.ID) {
			out = append(out, m)
		}
	}
	return out
}

// toHistory converts Discord's newest-first page into kojo's
// oldest-first history, dropping messages with nothing readable.
func toHistory(msgs []*discordgo.Message, parentID string) []HistoryMessage {
	out := make([]HistoryMessage, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m == nil || m.Author == nil {
			continue
		}
		text := strings.TrimSpace(m.Content)
		if n := len(m.Attachments); n > 0 {
			text = strings.TrimSpace(text + fmt.Sprintf(" [%d attachment(s)]", n))
		}
		if text == "" {
			continue
		}
		h := HistoryMessage{
			Platform:  "discord",
			ChannelID: m.ChannelID,
			MessageID: m.ID,
			UserID:    m.Author.ID,
			UserName:  sanitizeName(displayName(m.Author, m.Member)),
			Text:      text,
			Timestamp: m.Timestamp.UTC().Format(time.RFC3339),
			IsBot:     m.Author.Bot,
		}
		if parentID != "" {
			h.ThreadID = m.ChannelID
			h.ChannelID = parentID
		}
		out = append(out, h)
	}
	return out
}

// downloadAttachments saves the message's files into kojo's upload
// directory, the only place kojo accepts attachment paths from.
func (b *Bot) downloadAttachments(ctx context.Context, m *discordgo.Message) (saved []Attachment, skipped int) {
	if len(m.Attachments) == 0 {
		return nil, 0
	}
	if b.env.UploadDir == "" {
		return nil, len(m.Attachments)
	}
	if err := os.MkdirAll(b.env.UploadDir, 0o700); err != nil {
		return nil, len(m.Attachments)
	}
	for _, a := range m.Attachments {
		if a == nil || len(saved) >= maxAttachments || a.Size > maxAttachmentBytes {
			skipped++
			continue
		}
		path, size, err := b.download(ctx, a)
		if err != nil {
			b.log.Warn("attachment download failed", "name", a.Filename, "err", err)
			skipped++
			continue
		}
		saved = append(saved, Attachment{Path: path, Name: a.Filename, Mime: a.ContentType, Size: size})
	}
	return saved, skipped
}

func (b *Bot) download(ctx context.Context, a *discordgo.MessageAttachment) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download returned %s", resp.Status)
	}
	path := filepath.Join(b.env.UploadDir, uploadName(a.Filename))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxAttachmentBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxAttachmentBytes {
		err = errors.New("attachment is larger than the limit")
	}
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	return path, n, nil
}

// uploadName builds a collision-free file name that keeps a harmless
// version of the original, so the agent still sees the extension.
func uploadName(original string) string {
	var sb strings.Builder
	for _, r := range filepath.Base(original) {
		switch {
		case r >= '0' && r <= '9', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '.', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	safe := strings.TrimLeft(sb.String(), ".")
	if len(safe) > 80 {
		safe = safe[len(safe)-80:]
	}
	if safe == "" {
		safe = "file"
	}
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	return "discord-" + hex.EncodeToString(nonce[:]) + "-" + safe
}
