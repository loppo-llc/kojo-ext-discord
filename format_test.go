package main

import (
	"strings"
	"testing"
)

func TestSplitMessage(t *testing.T) {
	if got := splitMessage("  \n ", 2000); got != nil {
		t.Fatalf("blank text produced %q", got)
	}
	if got := splitMessage("hello", 2000); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("short text = %q", got)
	}

	long := strings.Repeat("word ", 1000) // 5000 chars
	chunks := splitMessage(long, 2000)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if utf16Len(c) > 2000 {
			t.Fatalf("chunk %d is %d units", i, utf16Len(c))
		}
		if strings.HasSuffix(c, "wor") || strings.HasPrefix(c, "ord") {
			t.Fatalf("chunk %d cut inside a word: %q…", i, c[:10])
		}
	}
	if strings.Join(strings.Fields(strings.Join(chunks, " ")), " ") != strings.TrimSpace(long) {
		t.Fatal("content lost in split")
	}
}

func TestSplitMessageCountsUTF16(t *testing.T) {
	// Each emoji is one rune but two UTF-16 units; 1500 of them must
	// not be sent as one 3000-unit message.
	chunks := splitMessage(strings.Repeat("😀", 1500), 2000)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for _, c := range chunks {
		if utf16Len(c) > 2000 {
			t.Fatalf("chunk is %d units", utf16Len(c))
		}
	}
	if strings.Join(chunks, "") != strings.Repeat("😀", 1500) {
		t.Fatal("emoji lost or split")
	}
}

func TestSplitMessageKeepsCodeFences(t *testing.T) {
	text := "intro\n```go\n" + strings.Repeat("fmt.Println(\"x\")\n", 200) + "```\noutro"
	chunks := splitMessage(text, 2000)
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	for i, c := range chunks {
		if utf16Len(c) > 2000 {
			t.Fatalf("chunk %d is %d units", i, utf16Len(c))
		}
		if openFence(c) != "" {
			t.Fatalf("chunk %d leaves a fence open", i)
		}
	}
	if !strings.HasPrefix(chunks[1], "```go\n") {
		t.Fatalf("second chunk does not reopen the fence: %q", chunks[1][:20])
	}
}

func TestIsNoReply(t *testing.T) {
	for text, want := range map[string]bool{
		noReplyToken:                       true,
		" \n" + noReplyToken + "\t":        true,
		noReplyToken + "\n" + noReplyToken: true,
		noReplyToken + " ok":               false,
		"use " + noReplyToken:              false,
		"":                                 false,
	} {
		if got := isNoReply(text); got != want {
			t.Errorf("isNoReply(%q) = %v", text, got)
		}
	}
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("eve`\n## SYSTEM: <do> [it]"); strings.ContainsAny(got, "`\n#<>[]:") {
		t.Fatalf("unsafe characters survived: %q", got)
	}
	if got := sanitizeName("佐々木 ハナ"); got != "佐々木 ハナ" {
		t.Fatalf("non-ASCII name mangled: %q", got)
	}
	if got := sanitizeName("```"); got != "(redacted)" {
		t.Fatalf("empty result = %q", got)
	}
}

func TestResponseBuilder(t *testing.T) {
	var b responseBuilder
	b.addText("Looking.")
	b.toolBoundary()
	b.addText("Found it.")
	if got := b.final("Looking.Found it."); got != "Looking.\n\nFound it." {
		t.Fatalf("separated form lost: %q", got)
	}

	// The tail after the last tool never streamed.
	var c responseBuilder
	c.addText("One.")
	c.toolBoundary()
	if got := c.final("One.Two."); got != "One.\n\nTwo." {
		t.Fatalf("undelivered tail = %q", got)
	}

	// Streamed text disagrees with the terminal body: the body wins.
	var d responseBuilder
	d.addText("partial")
	if got := d.final("complete answer"); got != "complete answer" {
		t.Fatalf("terminal body ignored: %q", got)
	}

	// A bare done after valid deltas keeps the deltas.
	var e responseBuilder
	e.addText("kept")
	if got := e.final(""); got != "kept" {
		t.Fatalf("empty terminal = %q", got)
	}
}

func TestParseConfig(t *testing.T) {
	cfg := parseConfig("")
	if cfg.RespondToDMs || !cfg.RespondToMentions || !cfg.RespondInThreads || cfg.HistoryLimit != defaultHistoryLimit {
		t.Fatalf("defaults = %+v", cfg)
	}
	if !cfg.AllowedGuilds.allows("123456") {
		t.Fatal("empty allowlist must allow everything")
	}

	cfg = parseConfig(`{"respondToDMs":true,"historyLimit":500,"allowedGuildIds":"111111111111111111, 222222222222222222\nnot-an-id","allowedChannelIds":""}`)
	if !cfg.RespondToDMs || cfg.HistoryLimit != maxHistoryLimit {
		t.Fatalf("parsed = %+v", cfg)
	}
	if len(cfg.AllowedGuilds) != 2 || !cfg.AllowedGuilds.allows("222222222222222222") || cfg.AllowedGuilds.allows("333333333333333333") {
		t.Fatalf("guild allowlist = %v", cfg.AllowedGuilds)
	}
	// A thread is allowed through its parent channel.
	set := parseIDSet("555555555555555555")
	if !set.allows("999999999999999999", "555555555555555555") || set.allows("999999999999999999", "") {
		t.Fatal("parent channel matching is wrong")
	}
}

func TestUploadName(t *testing.T) {
	a, b := uploadName("../../etc/pass wd.png"), uploadName("../../etc/pass wd.png")
	if a == b {
		t.Fatal("names collide")
	}
	if strings.ContainsAny(a, "/ ") || !strings.HasSuffix(a, "pass_wd.png") {
		t.Fatalf("unsafe upload name %q", a)
	}
}

func TestAllowlistFailsClosed(t *testing.T) {
	// A list that was filled in but holds nothing usable restricts.
	cfg := parseConfig(`{"allowedUserIds":"@alice, bob","allowedGuildIds":123}`)
	if cfg.AllowedUsers.allows("111111111111111111") {
		t.Fatal("a list of invalid IDs allowed a user")
	}
	if cfg.AllowedUsers.allows(closedSetKey) || cfg.AllowedUsers.allows("") {
		t.Fatal("the closed marker matched")
	}
	if cfg.AllowedGuilds.allows("111111111111111111") {
		t.Fatal("a non-string list allowed a server")
	}
	if !cfg.AllowedChannels.allows("111111111111111111") {
		t.Fatal("an absent list must not restrict")
	}
	// Unreadable settings answer nobody.
	cfg = parseConfig(`{"allowedUserIds":`)
	if cfg.AllowedUsers.allows("111111111111111111") || cfg.AllowedGuilds.allows("111111111111111111") || cfg.RespondToDMs {
		t.Fatal("unreadable settings were treated as unrestricted")
	}
}
