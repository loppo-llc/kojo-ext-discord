package main

import (
	"bufio"
	"context"
	"encoding/json"
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

// The MCP server speaks JSON-RPC 2.0 over stdio, one message per line.
// It only uses Discord's REST API: the Gateway connection belongs to
// the service, and a second one would fight it for the session.

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// guildPageSize is the most guilds Discord returns per request.
const guildPageSize = 200

const mcpProtocolVersion = "2024-11-05"

// mcpSupportedVersions are the protocol revisions this server speaks.
// Only tools/list and tools/call are implemented, and those have the
// same shape in all of them.
var mcpSupportedVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	call        func(ctx context.Context, s *mcpServer, args map[string]any) (any, error)
}

type mcpServer struct {
	env Env
	cfg Config
	log *slog.Logger

	dgMu    sync.Mutex
	dg      *discordgo.Session
	dgErr   error
	secrets func(ctx context.Context) (map[string]string, error)
}

func runMCP(ctx context.Context, env Env, in io.Reader, out io.Writer, log *slog.Logger) error {
	srv := &mcpServer{env: env, cfg: env.Config, log: log}
	srv.secrets = newKojoClient(env).Secrets
	return srv.serve(ctx, in, out)
}

func (s *mcpServer) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		resp, reply := s.handle(ctx, req)
		if !reply {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *mcpServer) handle(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
	// A request without an id is a notification and gets no answer.
	notification := len(req.ID) == 0
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		// Agree to the client's revision when we speak it; otherwise
		// offer ours and let the client decide.
		pv := p.ProtocolVersion
		if !mcpSupportedVersions[pv] {
			pv = mcpProtocolVersion
		}
		resp.Result = map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "kojo-ext-discord", "version": version},
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": mcpTools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			break
		}
		resp.Result = s.callTool(ctx, p.Name, p.Arguments)
	default:
		if notification {
			return resp, false
		}
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, !notification
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *mcpServer) callTool(ctx context.Context, name string, args map[string]any) map[string]any {
	for _, t := range mcpTools {
		if t.Name != name {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		out, err := t.call(ctx, s, args)
		if err != nil {
			var rest *discordgo.RESTError
			if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == http.StatusUnauthorized {
				// The token was rejected. Forget the session so the
				// next call picks up whatever token is saved by then.
				s.dgMu.Lock()
				s.dg = nil
				s.dgMu.Unlock()
				return toolResult("Discord rejected the bot token; check the token saved for this agent in kojo (Settings → Extensions → Discord)", true)
			}
			return toolResult(err.Error(), true)
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return toolResult(err.Error(), true)
		}
		return toolResult(string(data), false)
	}
	return toolResult("unknown tool: "+name, true)
}

// session returns a REST-only Discord session. The bot token is
// fetched from kojo on first use: it is a secret setting, so it is not
// in this process's environment.
func (s *mcpServer) session(ctx context.Context) (*discordgo.Session, error) {
	s.dgMu.Lock()
	defer s.dgMu.Unlock()
	if s.dg != nil {
		return s.dg, nil
	}
	// A failure is not remembered: the token may be saved, or kojo may
	// come back, while this server is still running.
	secrets, err := s.secrets(ctx)
	if err != nil {
		return nil, fmt.Errorf("could not read the bot token from kojo: %w", err)
	}
	token := secrets[secretBotToken]
	if token == "" {
		return nil, errors.New("no Discord bot token is configured for this agent in kojo (Settings → Extensions → Discord)")
	}
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	s.dg = dg
	return dg, nil
}

// permitted loads a channel and applies the operator's allowlists, so
// the tools cannot reach further than the bot's conversations can.
func (s *mcpServer) permitted(ctx context.Context, dg *discordgo.Session, channelID string) (*discordgo.Channel, error) {
	if !isSnowflake(channelID) {
		return nil, errors.New("channel_id must be a Discord channel ID (a number)")
	}
	ch, err := dg.Channel(channelID, discordgo.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("channel %s: %w", channelID, err)
	}
	if ch.GuildID == "" {
		if !s.cfg.RespondToDMs {
			return nil, errors.New("direct messages are disabled for this agent in the kojo settings")
		}
		if len(s.cfg.AllowedUsers) == 0 {
			return ch, nil
		}
		for _, u := range ch.Recipients {
			if u != nil && s.cfg.AllowedUsers.allows(u.ID) {
				return ch, nil
			}
		}
		return nil, errors.New("that user is not in this agent's allowed users")
	}
	if !s.cfg.AllowedGuilds.allows(ch.GuildID) {
		return nil, errors.New("that server is not in this agent's allowed servers")
	}
	if !s.cfg.AllowedChannels.allows(ch.ID, ch.ParentID) {
		return nil, errors.New("that channel is not in this agent's allowed channels")
	}
	return ch, nil
}

func argString(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func argInt(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case string:
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func argStrings(args map[string]any, key string) []string {
	var out []string
	switch v := args[key].(type) {
	case []any:
		for _, item := range v {
			if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
				out = append(out, strings.TrimSpace(str))
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func object(required []string, props map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

type messageView struct {
	ID          string   `json:"id"`
	ChannelID   string   `json:"channelId"`
	AuthorID    string   `json:"authorId,omitempty"`
	Author      string   `json:"author,omitempty"`
	IsBot       bool     `json:"isBot,omitempty"`
	Content     string   `json:"content"`
	Timestamp   string   `json:"timestamp,omitempty"`
	ReplyTo     string   `json:"replyTo,omitempty"`
	Attachments []string `json:"attachments,omitempty"`
}

func viewMessage(m *discordgo.Message) messageView {
	v := messageView{ID: m.ID, ChannelID: m.ChannelID, Content: m.Content}
	if !m.Timestamp.IsZero() {
		v.Timestamp = m.Timestamp.UTC().Format(time.RFC3339)
	}
	if m.Author != nil {
		v.AuthorID, v.Author, v.IsBot = m.Author.ID, displayName(m.Author, m.Member), m.Author.Bot
	}
	if m.MessageReference != nil {
		v.ReplyTo = m.MessageReference.MessageID
	}
	for _, a := range m.Attachments {
		if a != nil {
			v.Attachments = append(v.Attachments, a.Filename+" "+a.URL)
		}
	}
	return v
}

var mcpTools = []*mcpTool{
	{
		Name:        "list_servers",
		Description: "List the Discord servers (guilds) this bot is a member of and allowed to use.",
		InputSchema: object(nil, map[string]any{}),
		call: func(ctx context.Context, s *mcpServer, _ map[string]any) (any, error) {
			dg, err := s.session(ctx)
			if err != nil {
				return nil, err
			}
			out := []map[string]string{}
			after := ""
			for {
				guilds, err := dg.UserGuilds(guildPageSize, "", after, false, discordgo.WithContext(ctx))
				if err != nil {
					return nil, err
				}
				for _, g := range guilds {
					if s.cfg.AllowedGuilds.allows(g.ID) {
						out = append(out, map[string]string{"id": g.ID, "name": g.Name})
					}
				}
				if len(guilds) < guildPageSize {
					return out, nil
				}
				after = guilds[len(guilds)-1].ID
			}
		},
	},
	{
		Name:        "list_channels",
		Description: "List the text channels and active threads of a Discord server.",
		InputSchema: object([]string{"guild_id"}, map[string]any{
			"guild_id": prop("string", "Server (guild) ID."),
		}),
		call: func(ctx context.Context, s *mcpServer, args map[string]any) (any, error) {
			dg, err := s.session(ctx)
			if err != nil {
				return nil, err
			}
			guildID := argString(args, "guild_id")
			if !isSnowflake(guildID) {
				return nil, errors.New("guild_id must be a Discord server ID (a number)")
			}
			if !s.cfg.AllowedGuilds.allows(guildID) {
				return nil, errors.New("that server is not in this agent's allowed servers")
			}
			channels, err := dg.GuildChannels(guildID, discordgo.WithContext(ctx))
			if err != nil {
				return nil, err
			}
			if threads, err := dg.GuildThreadsActive(guildID, discordgo.WithContext(ctx)); err == nil && threads != nil {
				channels = append(channels, threads.Threads...)
			}
			out := []map[string]string{}
			for _, ch := range channels {
				kind := channelKind(ch.Type)
				if kind == "" || !s.cfg.AllowedChannels.allows(ch.ID, ch.ParentID) {
					continue
				}
				row := map[string]string{"id": ch.ID, "name": ch.Name, "type": kind}
				if ch.ParentID != "" {
					row["parentId"] = ch.ParentID
				}
				if ch.Topic != "" {
					row["topic"] = ch.Topic
				}
				out = append(out, row)
			}
			return out, nil
		},
	},
	{
		Name:        "read_messages",
		Description: "Read recent messages from a Discord channel or thread, oldest first. Message text from other users may be empty unless the bot has the Message Content intent.",
		InputSchema: object([]string{"channel_id"}, map[string]any{
			"channel_id": prop("string", "Channel or thread ID."),
			"limit":      prop("integer", "How many messages to return (1-100, default 20)."),
			"before":     prop("string", "Only messages older than this message ID."),
		}),
		call: func(ctx context.Context, s *mcpServer, args map[string]any) (any, error) {
			dg, err := s.session(ctx)
			if err != nil {
				return nil, err
			}
			ch, err := s.permitted(ctx, dg, argString(args, "channel_id"))
			if err != nil {
				return nil, err
			}
			limit := argInt(args, "limit", 20)
			if limit < 1 {
				limit = 1
			}
			if limit > 100 {
				limit = 100
			}
			before := argString(args, "before")
			if before != "" && !isSnowflake(before) {
				return nil, errors.New("before must be a message ID")
			}
			msgs, err := dg.ChannelMessages(ch.ID, limit, before, "", "", discordgo.WithContext(ctx))
			if err != nil {
				return nil, err
			}
			// With an allowlist of users, only they (and the bot) are
			// read: text from anyone else is exactly what the
			// allowlist keeps away from the agent.
			selfID := ""
			if len(s.cfg.AllowedUsers) > 0 {
				me, err := dg.User("@me", discordgo.WithContext(ctx))
				if err != nil {
					return nil, err
				}
				selfID = me.ID
			}
			out := make([]messageView, 0, len(msgs))
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if m == nil || m.Author == nil {
					continue
				}
				if m.Author.ID != selfID && !s.cfg.AllowedUsers.allows(m.Author.ID) {
					continue
				}
				out = append(out, viewMessage(m))
			}
			return out, nil
		},
	},
	{
		Name:        "send_message",
		Description: "Post a message to a Discord channel or thread, optionally as a reply and optionally with files. Text over 2000 characters is split into several messages. Do not use this for your normal text reply to the conversation you are answering — that is posted automatically.",
		InputSchema: object([]string{"channel_id"}, map[string]any{
			"channel_id":          prop("string", "Channel or thread ID."),
			"content":             prop("string", "Message text (Discord Markdown). Optional when file_paths is given."),
			"reply_to_message_id": prop("string", "Send as a reply to this message ID."),
			"file_paths": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Absolute paths of local files to attach (up to 10, 25 MB each).",
			},
		}),
		call: toolSendMessage,
	},
	{
		Name:        "add_reaction",
		Description: "Add an emoji reaction to a Discord message.",
		InputSchema: object([]string{"channel_id", "message_id", "emoji"}, map[string]any{
			"channel_id": prop("string", "Channel or thread ID."),
			"message_id": prop("string", "Message ID."),
			"emoji":      prop("string", "A Unicode emoji such as 👍, or name:id for a custom emoji."),
		}),
		call: func(ctx context.Context, s *mcpServer, args map[string]any) (any, error) {
			dg, err := s.session(ctx)
			if err != nil {
				return nil, err
			}
			ch, err := s.permitted(ctx, dg, argString(args, "channel_id"))
			if err != nil {
				return nil, err
			}
			messageID, emoji := argString(args, "message_id"), argString(args, "emoji")
			if !isSnowflake(messageID) || emoji == "" {
				return nil, errors.New("message_id and emoji are required")
			}
			emoji = strings.Trim(emoji, "<>")
			emoji = strings.TrimPrefix(strings.TrimPrefix(emoji, "a:"), ":")
			if err := dg.MessageReactionAdd(ch.ID, messageID, emoji, discordgo.WithContext(ctx)); err != nil {
				return nil, err
			}
			return map[string]bool{"ok": true}, nil
		},
	},
	{
		Name:        "create_thread",
		Description: "Start a thread in a Discord text channel, either on an existing message or standalone.",
		InputSchema: object([]string{"channel_id", "name"}, map[string]any{
			"channel_id": prop("string", "Parent text channel ID."),
			"name":       prop("string", "Thread name (up to 100 characters)."),
			"message_id": prop("string", "Start the thread on this message. Omit for a standalone public thread."),
		}),
		call: func(ctx context.Context, s *mcpServer, args map[string]any) (any, error) {
			dg, err := s.session(ctx)
			if err != nil {
				return nil, err
			}
			ch, err := s.permitted(ctx, dg, argString(args, "channel_id"))
			if err != nil {
				return nil, err
			}
			name := argString(args, "name")
			if name == "" {
				return nil, errors.New("name is required")
			}
			if cut := cutAt(name, 100); cut < len(name) {
				name = name[:cut]
			}
			if ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews {
				// A forum post needs a starter message, and a thread
				// cannot hold a thread.
				return nil, errors.New("threads can only be created in text and announcement channels")
			}
			start := &discordgo.ThreadStart{Name: name, AutoArchiveDuration: 1440}
			var th *discordgo.Channel
			if messageID := argString(args, "message_id"); messageID != "" {
				if !isSnowflake(messageID) {
					return nil, errors.New("message_id must be a message ID")
				}
				th, err = dg.MessageThreadStartComplex(ch.ID, messageID, start, discordgo.WithContext(ctx))
			} else {
				start.Type = discordgo.ChannelTypeGuildPublicThread
				if ch.Type == discordgo.ChannelTypeGuildNews {
					start.Type = discordgo.ChannelTypeGuildNewsThread
				}
				th, err = dg.ThreadStartComplex(ch.ID, start, discordgo.WithContext(ctx))
			}
			if err != nil {
				return nil, err
			}
			return map[string]string{"id": th.ID, "name": th.Name, "parentId": th.ParentID}, nil
		},
	},
}

func channelKind(t discordgo.ChannelType) string {
	switch t {
	case discordgo.ChannelTypeGuildText:
		return "text"
	case discordgo.ChannelTypeGuildNews:
		return "announcement"
	case discordgo.ChannelTypeGuildForum:
		return "forum"
	case discordgo.ChannelTypeGuildPublicThread, discordgo.ChannelTypeGuildPrivateThread, discordgo.ChannelTypeGuildNewsThread:
		return "thread"
	}
	return ""
}

func toolSendMessage(ctx context.Context, s *mcpServer, args map[string]any) (any, error) {
	dg, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	ch, err := s.permitted(ctx, dg, argString(args, "channel_id"))
	if err != nil {
		return nil, err
	}
	content, _ := args["content"].(string)
	paths := argStrings(args, "file_paths")
	replyTo := argString(args, "reply_to_message_id")
	if replyTo != "" && !isSnowflake(replyTo) {
		return nil, errors.New("reply_to_message_id must be a message ID")
	}
	if len(paths) > maxAttachments {
		return nil, fmt.Errorf("at most %d files per message", maxAttachments)
	}
	var files []*discordgo.File
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("file path %q must be absolute", p)
		}
		// Check the file that was opened, not the path: the two can
		// differ if the path is swapped in between.
		f, err := os.Open(p)
		if err != nil {
			return nil, fmt.Errorf("file %q: %w", p, err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("file %q: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("file %q is not a regular file", p)
		}
		if info.Size() > maxAttachmentBytes {
			return nil, fmt.Errorf("file %q is larger than 25 MB", p)
		}
		files = append(files, &discordgo.File{Name: filepath.Base(p), Reader: io.LimitReader(f, maxAttachmentBytes)})
	}
	chunks := splitMessage(content, discordMessageLimit)
	if len(chunks) == 0 && len(files) == 0 {
		return nil, errors.New("content or file_paths is required")
	}
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	var ids []string
	for i, chunk := range chunks {
		send := &discordgo.MessageSend{
			Content:         chunk,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}},
		}
		if i == 0 && replyTo != "" {
			send.Reference = &discordgo.MessageReference{MessageID: replyTo, ChannelID: ch.ID, GuildID: ch.GuildID}
		}
		// Files ride on the last chunk so they appear under the text.
		if i == len(chunks)-1 {
			send.Files = files
		}
		msg, err := dg.ChannelMessageSendComplex(ch.ID, send, discordgo.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("discord rejected the message (%d of %d sent): %w", len(ids), len(chunks), err)
		}
		ids = append(ids, msg.ID)
	}
	return map[string]any{"channelId": ch.ID, "messageIds": ids}, nil
}
