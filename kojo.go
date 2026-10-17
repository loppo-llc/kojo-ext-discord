package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// KojoClient talks to the kojo API with the extension's own token.
type KojoClient struct {
	base    string
	token   string
	agentID string
	// stream has no overall timeout: a turn runs as long as the agent
	// needs. Liveness comes from the server's 15s ping lines instead.
	stream *http.Client
	short  *http.Client
}

func newKojoClient(e Env) *KojoClient {
	return &KojoClient{
		base:    e.APIBase,
		token:   e.Token,
		agentID: e.AgentID,
		stream:  &http.Client{},
		short:   &http.Client{Timeout: 30 * time.Second},
	}
}

// HistoryMessage mirrors kojo's chathistory.HistoryMessage.
type HistoryMessage struct {
	Platform  string `json:"platform"`
	ChannelID string `json:"channelId"`
	ThreadID  string `json:"threadId,omitempty"`
	MessageID string `json:"messageId"`
	UserID    string `json:"userId"`
	UserName  string `json:"userName,omitempty"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	IsBot     bool   `json:"isBot,omitempty"`
}

// Attachment is a file already written into KOJO_UPLOAD_DIR.
type Attachment struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
	Mime string `json:"mime,omitempty"`
	Size int64  `json:"size,omitempty"`
}

type ChatRequest struct {
	SessionKey        string           `json:"sessionKey"`
	Message           string           `json:"message"`
	SystemPromptExtra string           `json:"systemPromptExtra,omitempty"`
	History           []HistoryMessage `json:"history,omitempty"`
	HistorySelfUserID string           `json:"historySelfUserId,omitempty"`
	Attachments       []Attachment     `json:"attachments,omitempty"`
}

// ChatEvent is the subset of kojo's agent.ChatEvent the bridge reads.
type ChatEvent struct {
	Type            string `json:"type"`
	Delta           string `json:"delta,omitempty"`
	ErrorMessage    string `json:"errorMessage,omitempty"`
	ParentToolUseID string `json:"parentToolUseId,omitempty"`
	Message         *struct {
		Content string `json:"content"`
	} `json:"message,omitempty"`
}

// APIError is a non-2xx answer from kojo.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kojo API %d %s: %s", e.Status, e.Code, e.Message)
}

func apiErrorCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

func decodeAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	out := &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) == nil && env.Error.Code != "" {
		out.Code, out.Message = env.Error.Code, env.Error.Message
	}
	return out
}

func (c *KojoClient) post(ctx context.Context, client *http.Client, path string, body any) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)
	return client.Do(req)
}

// streamIdleTimeout is how long the stream may stay silent. kojo
// writes a ping every 15s, so a minute of nothing means the connection
// is dead even if TCP has not noticed.
const streamIdleTimeout = 75 * time.Second

// Chat runs one turn and calls onEvent for every event until the
// stream ends. It returns nil once a terminal event (done or error)
// was seen; the caller reads the outcome from the events themselves.
//
// A ctx cancelled with errStoppedByUser (a `!stop`) first tells kojo the
// user stopped the turn (ext-chat/cancel with byUser), then drops the
// stream; any other cancel just drops it (kojo counts that as its own stop).
func (c *KojoClient) Chat(parent context.Context, req ChatRequest, onEvent func(ChatEvent)) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	defer cancel()
	stop := context.AfterFunc(parent, func() {
		if errors.Is(context.Cause(parent), errStoppedByUser) {
			cctx, done := context.WithTimeout(ctx, cancelRequestTimeout)
			// On failure the dropped stream still stops the turn.
			_ = c.cancelTurn(cctx, req.SessionKey)
			done()
		}
		cancel()
	})
	defer stop()
	resp, err := c.post(ctx, c.stream, "/api/v1/agents/"+url.PathEscape(c.agentID)+"/ext-chat", req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}

	idle := time.AfterFunc(streamIdleTimeout, cancel)
	defer idle.Stop()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	terminal := false
	for sc.Scan() {
		idle.Reset(streamIdleTimeout)
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var evt ChatEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Type == "ping" {
			continue
		}
		if evt.Type == "done" || evt.Type == "error" {
			terminal = true
		}
		onEvent(evt)
	}
	if err := sc.Err(); err != nil && !terminal {
		return err
	}
	if !terminal {
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the stream ended before the turn completed")
	}
	return nil
}

// errStoppedByUser is the cancel cause of a turn stopped by `!stop`.
var errStoppedByUser = errors.New("turn stopped by the user")

// cancelRequestTimeout bounds the ext-chat/cancel request.
const cancelRequestTimeout = 5 * time.Second

// cancelTurn stops sessionKey's turn as an explicit stop by a person.
func (c *KojoClient) cancelTurn(ctx context.Context, sessionKey string) error {
	resp, err := c.post(ctx, c.short, "/api/v1/agents/"+url.PathEscape(c.agentID)+"/ext-chat/cancel",
		map[string]any{"sessionKey": sessionKey, "byUser": true})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}
	return nil
}

// Steer injects text into the turn already running for sessionKey.
func (c *KojoClient) Steer(ctx context.Context, sessionKey, content string) error {
	resp, err := c.post(ctx, c.short, "/api/v1/agents/"+url.PathEscape(c.agentID)+"/ext-chat/steer",
		map[string]string{"sessionKey": sessionKey, "content": content})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeAPIError(resp)
	}
	return nil
}

// Secrets fetches the extension's secret settings. The MCP server uses
// it; the service already has them in KOJO_EXT_SECRETS.
func (c *KojoClient) Secrets(ctx context.Context) (map[string]string, error) {
	u := c.base + "/api/v1/extension/secrets"
	if c.agentID != "" {
		u += "?agentId=" + url.QueryEscape(c.agentID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.short.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeAPIError(resp)
	}
	var out struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}
