package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"time"
)

// Config is the per-agent settings object kojo hands over in
// KOJO_EXT_CONFIG. Every field has a default so an agent that was
// bound without touching the form still behaves sensibly.
type Config struct {
	RespondToDMs         bool
	RespondToMentions    bool
	RespondInThreads     bool
	ReplyInThread        bool
	MessageContentIntent bool
	RespondToReactions   bool
	HistoryLimit         int
	BatchWindow          time.Duration
	AllowedGuilds        idSet
	AllowedChannels      idSet
	AllowedUsers         idSet
}

// idSet is an allowlist of Discord snowflakes. An empty set allows
// everything: the settings form has no array type, so the lists arrive
// as comma-separated strings and "left blank" has to mean "no limit".
type idSet map[string]struct{}

func (s idSet) allows(ids ...string) bool {
	if len(s) == 0 {
		return true
	}
	for _, id := range ids {
		if _, ok := s[id]; ok && isSnowflake(id) {
			return true
		}
	}
	return false
}

// closedSetKey marks a list that restricts but matches nothing. It is
// not a snowflake, so allows never accepts it.
const closedSetKey = "invalid"

// closedSet allows nobody.
func closedSet() idSet { return idSet{closedSetKey: {}} }

// parseIDSet reads a comma-separated ID list. Anything that is not an
// ID is dropped — but a list the operator filled in keeps restricting
// even when nothing in it is usable: a typo must not turn "only these"
// into "everyone".
func parseIDSet(v any) idSet {
	if v == nil {
		return idSet{}
	}
	str, ok := v.(string)
	if !ok {
		return closedSet()
	}
	out := idSet{}
	parts := strings.FieldsFunc(str, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == ';'
	})
	for _, part := range parts {
		if isSnowflake(part) {
			out[part] = struct{}{}
		}
	}
	if len(out) == 0 && strings.TrimSpace(str) != "" {
		return closedSet()
	}
	return out
}

func isSnowflake(s string) bool {
	if len(s) < 5 || len(s) > 25 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

const (
	defaultHistoryLimit = 20
	maxHistoryLimit     = 100
	defaultBatchSeconds = 2
	maxBatchSeconds     = 30
)

func parseConfig(raw string) Config {
	m := map[string]any{}
	unreadable := false
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			unreadable = true
			m = map[string]any{}
		}
	}
	boolOr := func(key string, def bool) bool {
		if v, ok := m[key].(bool); ok {
			return v
		}
		return def
	}
	cfg := Config{
		RespondToDMs:         boolOr("respondToDMs", false),
		RespondToMentions:    boolOr("respondToMentions", true),
		RespondInThreads:     boolOr("respondInThreads", true),
		ReplyInThread:        boolOr("replyInThread", false),
		MessageContentIntent: boolOr("messageContentIntent", false),
		RespondToReactions:   boolOr("respondToReactions", true),
		HistoryLimit:         defaultHistoryLimit,
		AllowedGuilds:        parseIDSet(m["allowedGuildIds"]),
		AllowedChannels:      parseIDSet(m["allowedChannelIds"]),
		AllowedUsers:         parseIDSet(m["allowedUserIds"]),
	}
	if v, ok := m["historyLimit"].(float64); ok {
		cfg.HistoryLimit = int(v)
	}
	if cfg.HistoryLimit < 0 {
		cfg.HistoryLimit = 0
	}
	if cfg.HistoryLimit > maxHistoryLimit {
		cfg.HistoryLimit = maxHistoryLimit
	}
	batch := float64(defaultBatchSeconds)
	if v, ok := m["batchWindowSeconds"].(float64); ok {
		batch = v
	}
	batch = math.Max(0, math.Min(batch, maxBatchSeconds))
	cfg.BatchWindow = time.Duration(batch * float64(time.Second))
	if unreadable {
		// Settings that cannot be read may have held allowlists. Answer
		// nobody rather than everybody.
		cfg.AllowedGuilds, cfg.AllowedChannels, cfg.AllowedUsers = closedSet(), closedSet(), closedSet()
		cfg.RespondToDMs = false
	}
	return cfg
}

// Env is the KOJO_EXT_* contract, read once at startup.
type Env struct {
	APIBase   string
	Token     string
	TokenFile string
	AgentID   string
	DataDir   string
	UploadDir string
	Config    Config
	Secrets   map[string]string
}

func loadEnv() Env {
	e := Env{
		APIBase:   strings.TrimRight(os.Getenv("KOJO_API_BASE"), "/"),
		Token:     os.Getenv("KOJO_EXT_TOKEN"),
		TokenFile: os.Getenv("KOJO_EXT_TOKEN_FILE"),
		AgentID:   os.Getenv("KOJO_EXT_AGENT_ID"),
		DataDir:   os.Getenv("KOJO_EXT_DATA_DIR"),
		UploadDir: os.Getenv("KOJO_UPLOAD_DIR"),
		Config:    parseConfig(os.Getenv("KOJO_EXT_CONFIG")),
		Secrets:   map[string]string{},
	}
	if raw := os.Getenv("KOJO_EXT_SECRETS"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &e.Secrets)
	}
	// An MCP server gets the token as a file only: its environment is
	// assembled on the backend CLI's command line, where `ps` can see it.
	if e.Token == "" && e.TokenFile != "" {
		if data, err := os.ReadFile(e.TokenFile); err == nil {
			e.Token = strings.TrimSpace(string(data))
		}
	}
	return e
}

const secretBotToken = "botToken"
