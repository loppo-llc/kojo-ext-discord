# kojo-ext-discord

[日本語](README.ja.md)

An extension package that connects a [kojo](https://github.com/loppo-llc/kojo) agent to Discord.

- **Mention** the bot in a server channel, or **reply** to one of its messages, and the agent answers
- In a **thread** the bot has already spoken in, the conversation continues without mentions
- It can answer **DMs** (off by default)
- **Attachments** on a message are handed to the agent
- **Reactions** (emoji) on the bot's messages are passed to the agent too
- Messages and reactions that pile up within a moment reach the agent **together, as one turn**
- The agent can also **post on its own** (send files, react, create threads, read history)
- Each Discord channel / thread is its own session, separate from the chat in the kojo UI

One Discord bot per agent. To connect three agents, create three bots.

## Requirements

- A kojo build **newer than v0.135.0** (needs the `chat:session` scope and extension secret settings)
- A Discord account, and a server where you have the "Manage Server" permission needed to invite a bot

## 1. Discord setup

### 1-1. Create an application

1. Open <https://discord.com/developers/applications> and log in
2. Click **New Application** in the top right
3. Enter a name (it becomes the bot's initial display name; you can change it later), accept the terms, and click **Create**
4. On the **General Information** page that opens, note the **Application ID** (used in the invite URL)

### 1-2. Issue a bot token

1. Open **Bot** in the left menu
2. Click **Reset Token**, then **Yes, do it!** in the confirmation dialog (you are asked for a code if you use two-factor authentication)
3. **Copy** the token that appears

> **The token is shown only this once.** If you lose it, the only way forward is another Reset Token (which invalidates the old one).
> The token is the bot's password. Do not paste it into chats or repositories. If it leaks, Reset Token immediately.

The bot's icon and username can be changed on the same page.

### 1-3. Stop strangers from inviting the bot (recommended)

By default, anyone who knows the invite URL can add this bot to their own server. The agent can run tools, so you do not want it in servers you do not know.

1. Open **Installation** in the left menu, set **Install Link** to **None**, and save
2. Go back to the **Bot** page, turn **Public Bot** off, and save

(Public Bot cannot be turned off while Install Link is anything other than None.)

### 1-4. Message Content Intent (only if you need it)

Discord restricts which message bodies a bot can read. **Without** the Message Content Intent a bot can read only:

- messages that mention the bot
- DMs sent to the bot
- the bot's own messages

So if all you want is "answer when mentioned", you do not need this intent. You need it for:

| Feature | Message Content Intent |
| --- | --- |
| Answering mentions, answering DMs | not needed |
| Answering follow-ups in a thread without a mention | **needed** |
| Answering replies sent with the @ ping turned off | **needed** |
| Passing earlier channel messages as context when a conversation starts | **needed** (without it, other people's messages arrive empty) |

To enable it:

1. On the **Bot** page, under **Privileged Gateway Intents**, turn on **Message Content Intent** and click **Save Changes**
2. Later, also turn on "Use the Message Content intent" in the kojo settings (it only takes effect when both are on)

A bot in fewer than 100 servers can simply flip the switch. At 100 or more, Discord requires a review.

**Presence Intent** and **Server Members Intent** are not used. Leave them off.

### 1-5. Invite the bot to your server

Replace `YOUR_APPLICATION_ID` in this URL with the Application ID from 1-1 and open it in a browser:

```
https://discord.com/oauth2/authorize?client_id=YOUR_APPLICATION_ID&scope=bot&permissions=309237763136
```

Pick the server, review the permissions, and click **Authorize**. The bot appearing (offline) in the server's member list means it worked.

`permissions=309237763136` stands for:

| Permission | Used for |
| --- | --- |
| View Channels | receiving messages |
| Send Messages | answering |
| Send Messages in Threads | answering inside threads |
| Create Public Threads | "Answer in a new thread", the `create_thread` tool |
| Read Message History | answering as a reply, using earlier messages as context |
| Attach Files | the agent sending files |
| Embed Links | link previews for URLs in an answer |
| Add Reactions | acknowledgement marks (👀 and so on), the `add_reaction` tool |

If you would rather not build the URL by hand, **OAuth2 → URL Generator** in the Developer Portal gives the same URL: select `bot` under **Scopes** and tick the permissions above under **Bot Permissions**.

Administrator is not needed. Do not grant it.

> To use the bot in a private channel, add the bot (or its role) in that channel's permission settings.

### 1-6. Finding IDs (for the allowlists)

Restricting which users / servers / channels the bot answers requires their IDs.

1. In Discord, turn on **User Settings → Advanced → Developer Mode**
2. Right-click (long-press on mobile) a user, a server icon, or a channel name and choose **Copy ID**

## 2. kojo setup

1. Open **Settings → Extensions** in kojo
2. Enter `https://github.com/loppo-llc/kojo-ext-discord` as the **repository URL** and click the button that inspects it
3. Review the requested scope (`chat:session` — talk to the agent in a dedicated session) and approve the install
4. Open **Discord** in the installed list and enable it for the agent you want to connect
5. Paste the token from 1-2 into **Bot token** in that agent's settings and save

Saving starts the service for that agent, and the bot comes online in Discord within a few seconds. Mention the bot in a channel and it answers.

The token is stored encrypted in kojo's credential store and never appears again in the UI or the API. To replace it, enter a new value and save; to delete it, press the remove button and save.

### Settings

| Setting | Default | Description |
| --- | --- | --- |
| Bot token | — | Required. The bot's token |
| Answer mentions and replies | on | Answer mentions / replies to the bot in server channels |
| Keep answering in threads | on | In a thread the bot has spoken in, answer without a mention. Needs the Message Content Intent |
| Answer in a new thread | off | When mentioned, create a thread on that message and answer there. Each thread is a separate conversation. When off, the bot replies in the channel and the whole channel is one conversation |
| Notify the agent of reactions | on | Tell the agent when someone adds an emoji reaction to one of the bot's messages. The agent decides whether an answer is called for. Notifications cost agent turns (ones that pile up share a turn; while the agent is working they go into the running turn) |
| Answer direct messages | off | Answer DMs. **Read the warning below** |
| Use the Message Content intent | off | Use the Message Content Intent. Enable it in the Developer Portal too (1-4) |
| Channel history as context | 20 | How many earlier channel messages to give the agent when a conversation starts (0–100, 0 disables) |
| Batch window (seconds) | 2 | Messages / reactions that arrive in the same conversation within this many seconds of the first one are handed to the agent together (0–30). 0 hands each over as soon as it arrives |
| Allowed users | empty | User IDs to answer (comma-separated). Empty means everyone. A list holding nothing that reads as an ID answers nobody (the same goes for the other allowlists) |
| Allowed servers | empty | Server IDs to answer in (comma-separated). Empty means every server the bot is in |
| Allowed channels | empty | Channel IDs to answer in (comma-separated). Threads follow their parent channel. Empty means every channel the bot can see |

### Mind who can talk to it

Anyone who talks to the bot on Discord **can give instructions to that agent** — including whatever shell and file access the agent has. Strongly recommended:

- Only add the bot to servers where everyone is trusted. Always do the Public Bot step in 1-3
- In a server with people you do not fully trust, put your own ID (and those of people you trust) in **Allowed users**
- If you turn on **Answer direct messages**, always set **Allowed users** as well. Anyone who shares a server with the bot can DM it

## Usage

- **Talk to it**: write `@botname hello` in a channel. Replying to one of the bot's messages works too
- **React**: put an emoji on one of the bot's messages and the agent is told who reacted to which message with what. The bot stays silent when no answer is needed. Reactions to other people's messages and removed reactions are not passed on. In a DM they are passed on only while **Answer direct messages** is on
- **Add to a running turn**: write again in the same conversation while the agent is working and the message is handed to the running turn as a follow-up instruction (marked 👀). Backends that do not support this queue it instead, and everything queued becomes the next single turn
- **Things that arrive together**: messages and reactions that come in quick succession in one conversation are collected for the **Batch window** and handed over in one go. They get a single answer, posted as a reply to the newest message. At most 10 attachments across the batch reach the agent. A conversation holds at most 20 of them; messages beyond that are marked ⏳ and dropped
- **Stop**: write `@botname !stop` while it is working (just `!stop` in a DM or a followed thread). It is marked 🛑 and stops
- **Conversation scope**: one session per channel (or thread, or DM). It does not remember conversations from other channels, though the agent's own memory files are shared
- **Sometimes it stays silent**: when the agent decides no answer is needed, nothing is posted

### Tools the agent gets

An agent with this extension enabled gains these tools from the MCP server `discord_bot`. They work from the kojo UI and from cron too, so "post a summary to #news every morning" is possible.

| Tool | What it does |
| --- | --- |
| `list_servers` | List the servers the bot is in |
| `list_channels` | List a server's text channels and active threads |
| `read_messages` | Read recent messages in a channel / thread (with the reactions on them and their counts) |
| `send_message` | Post a message. Can reply to a message and attach files (up to 10, 25 MB each) |
| `add_reaction` | React to a message |
| `create_thread` | Create a thread |

The tools obey **Allowed servers / Allowed channels** too. When **Allowed users** is set, reaction notifications come only from those users, `read_messages` and the history passed at the start of a conversation contain only messages from those users and the bot itself, and DMs can be read or sent only with those users. `create_thread` works in text and announcement channels only.

## Troubleshooting

| Symptom | Check |
| --- | --- |
| The bot stays offline | Was the Bot token saved? Are both the extension and the agent enabled? `Discord rejected the bot token` in the kojo log means the token is wrong — Reset Token and try again |
| No reaction to a mention | Can the bot see that channel (add it to private channels)? Is it blocked by Allowed users / servers / channels? Is `@botname` a mention of a role with the same name rather than the bot? |
| No reaction to follow-ups in a thread | Is the Message Content Intent on in both the Developer Portal and kojo? Has the bot spoken in that thread at least once? |
| It answers but ignores earlier messages | With the Message Content Intent off, it cannot read other people's messages |
| `Missing Permissions` / `Missing Access` in the log | The invite permissions, or a per-channel permission override, deny the bot |
| A message starting with ⚠️ comes back | An error on the agent side; the text says why. Check the kojo log too |

Logs go to kojo's own log, tagged `extension=discord`.

## Limitations

- No slash commands, no button-driven questions, no voice channels
- The answer is posted in one go when the turn finishes ("typing…" shows meanwhile)
- Discord limits a message to 2000 characters; longer answers are split automatically. Discord does not render tables
- Forum channel posts are treated as threads

## Development

```sh
make test    # go vet + go test
make build   # build binaries for every platform under bin/
```

kojo installs an extension package by `git clone` and does not build it, so the binaries are committed to the repository. After changing code, run `make build`, commit `bin/` along with it, and bump `version` in `kojo-package.json`.

The same binary plays two roles: with no arguments it is the service that stays on the Gateway; with `mcp` it is a stdio MCP server.

## License

MIT
