// kojo-ext-discord bridges a Discord bot account to a kojo agent.
//
// The same binary runs in two modes:
//
//	kojo-ext-discord        the supervised service: holds the Gateway
//	                        connection and turns Discord messages into
//	                        agent turns
//	kojo-ext-discord mcp    a stdio MCP server the agent uses to act on
//	                        Discord on its own initiative
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

// version is stamped by the Makefile.
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	var err error
	switch mode {
	case "":
		err = runService(ctx, loadEnv(), log)
	case "mcp":
		err = runMCP(ctx, loadEnv(), os.Stdin, os.Stdout, log)
	case "version", "--version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown mode %q", mode)
	}
	if err != nil {
		log.Error("kojo-ext-discord stopped", "err", err)
		os.Exit(1)
	}
}

func runService(ctx context.Context, env Env, log *slog.Logger) error {
	if env.APIBase == "" || env.Token == "" || env.AgentID == "" {
		return errors.New("KOJO_API_BASE, KOJO_EXT_TOKEN and KOJO_EXT_AGENT_ID are required; this binary is started by kojo")
	}
	log = log.With("agent", env.AgentID)
	token := env.Secrets[secretBotToken]
	if token == "" {
		// Not an error worth a restart loop: kojo restarts the service
		// by itself once the operator saves a token.
		log.Warn("no bot token configured; waiting. Set it in kojo: Settings → Extensions → Discord → this agent")
		<-ctx.Done()
		return nil
	}

	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return err
	}
	me, err := dg.User("@me", discordgo.WithContext(ctx))
	if err != nil {
		var rest *discordgo.RESTError
		if errors.As(err, &rest) && rest.Response != nil && rest.Response.StatusCode == 401 {
			log.Error("Discord rejected the bot token (401). Reset it in the Developer Portal and save the new one in kojo")
			<-ctx.Done()
			return nil
		}
		return fmt.Errorf("discord login check: %w", err)
	}

	cfg := env.Config
	intents := discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages
	if cfg.MessageContentIntent {
		if messageContentGranted(ctx, dg, log) {
			intents |= discordgo.IntentMessageContent
		} else {
			// Identifying with an intent the application does not have
			// closes the Gateway with 4014 and the bot never connects.
			log.Warn("the Message Content intent is enabled in kojo but not in the Discord Developer Portal; continuing without it")
		}
	}
	dg.Identify.Intents = intents
	dg.StateEnabled = true

	bot := newBot(env, dg, log)
	bot.selfID = me.ID
	dg.AddHandler(bot.onMessageCreate)
	dg.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		log.Info("connected to Discord", "bot", r.User.Username, "guilds", len(r.Guilds))
	})
	if err := dg.Open(); err != nil {
		return fmt.Errorf("discord gateway: %w", err)
	}
	defer dg.Close()

	<-ctx.Done()
	return nil
}

// Application flags that say the Message Content intent is available.
const (
	flagMessageContent        = 1 << 18
	flagMessageContentLimited = 1 << 19
)

func messageContentGranted(ctx context.Context, dg *discordgo.Session, log *slog.Logger) bool {
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(2 * time.Second):
			}
		}
		app, err := dg.Application("@me")
		if err != nil {
			log.Debug("application lookup failed", "err", err)
			continue
		}
		return app.Flags&(flagMessageContent|flagMessageContentLimited) != 0
	}
	// Cannot tell. Asking for an intent the application lacks keeps the
	// bot offline; going without it only narrows what it can read.
	log.Warn("could not check whether the Message Content intent is enabled; continuing without it")
	return false
}
