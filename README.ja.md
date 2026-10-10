# kojo-ext-discord

[English](README.md)

[kojo](https://github.com/loppo-llc/kojo) のエージェントを Discord につなぐ拡張パッケージ。

- サーバーのチャンネルで bot を **メンション** するか、bot の発言に **リプライ** すると、エージェントが返信する
- 一度 bot が発言した **スレッド** では、以降メンションなしで会話が続く
- **DM** にも応答できる (既定はオフ)
- メッセージに付いた **添付ファイル** はエージェントに渡る
- エージェント側からも **自発的に投稿** できる (ファイル送信、リアクション、スレッド作成、履歴の読み取り)
- 会話は Discord のチャンネル / スレッド単位で独立したセッションになり、kojo の画面上のチャットとは混ざらない

1 エージェントにつき 1 つの Discord bot を割り当てる。エージェントを 3 体つなぎたければ bot も 3 つ作る。

## 必要なもの

- kojo **v0.135.0 より新しいビルド** (`chat:session` 権限と拡張機能の secret 設定が必要)
- Discord アカウントと、bot を招待できる権限 (「サーバーの管理」) を持つサーバー

## 1. Discord 側の準備

### 1-1. アプリケーションを作る

1. <https://discord.com/developers/applications> を開いてログインする
2. 右上の **New Application** を押す
3. 名前を入れる (これが bot の表示名の初期値になる。あとで変えられる)。規約に同意して **Create**
4. 開いた **General Information** ページの **Application ID** を控えておく (招待 URL で使う)

### 1-2. bot トークンを発行する

1. 左メニューの **Bot** を開く
2. **Reset Token** を押し、確認ダイアログで **Yes, do it!** (2 段階認証を有効にしていればコードを求められる)
3. 表示されたトークンを **Copy** する

> **トークンはこの 1 回しか表示されない。** 見失ったらもう一度 Reset Token するしかない (古いトークンは無効になる)。
> トークンは bot のパスワードそのもの。チャットやリポジトリに貼らない。漏れたらすぐ Reset Token する。

同じページで bot のアイコンとユーザー名も変えられる。

### 1-3. 他人に勝手に招待されないようにする (推奨)

既定では、招待 URL を知っていれば誰でもこの bot を自分のサーバーに入れられる。エージェントはツールを実行できるので、知らないサーバーに入れられるのは避けたい。

1. 左メニューの **Installation** を開き、**Install Link** を **None** にして保存する
2. **Bot** ページに戻り、**Public Bot** をオフにして保存する

(Install Link が None 以外のままだと Public Bot をオフにできない)

### 1-4. Message Content Intent (必要な場合だけ)

Discord は、bot が読めるメッセージ本文を制限している。Message Content Intent **なし** でも読めるのは次の 3 つだけ:

- bot をメンションしたメッセージ
- bot 宛ての DM
- bot 自身のメッセージ

つまり「メンションされたら答える」だけなら、この intent は不要。次の機能を使うなら必要になる:

| 機能 | Message Content Intent |
| --- | --- |
| メンションへの応答、DM への応答 | 不要 |
| スレッド内でメンションなしの続きに応答する | **必要** |
| 通知オフ (@ なし) のリプライに応答する | **必要** |
| 会話開始時にチャンネルの過去ログを文脈として渡す | **必要** (なしだと他人の発言の本文が空になる) |

有効にする手順:

1. **Bot** ページの **Privileged Gateway Intents** にある **Message Content Intent** をオンにして **Save Changes**
2. あとで kojo 側の設定「Use the Message Content intent」もオンにする (両方オンで初めて効く)

参加サーバーが 100 未満の bot はスイッチを入れるだけで使える。100 以上になると Discord の審査が必要。

**Presence Intent** と **Server Members Intent** は使わない。オフのままでいい。

### 1-5. サーバーに招待する

次の URL の `YOUR_APPLICATION_ID` を 1-1 で控えた Application ID に置き換え、ブラウザで開く:

```
https://discord.com/oauth2/authorize?client_id=YOUR_APPLICATION_ID&scope=bot&permissions=309237763136
```

招待先のサーバーを選び、権限を確認して **認証** を押す。サーバーのメンバー一覧に bot が (オフライン状態で) 現れれば成功。

`permissions=309237763136` は次の権限に対応している:

| 権限 | 用途 |
| --- | --- |
| チャンネルを見る (View Channels) | メッセージを受け取る |
| メッセージを送信 (Send Messages) | 返信する |
| スレッドでメッセージを送信 (Send Messages in Threads) | スレッド内で返信する |
| 公開スレッドの作成 (Create Public Threads) | 「Answer in a new thread」、`create_thread` ツール |
| メッセージ履歴を読む (Read Message History) | リプライとして返信する、過去ログを文脈に使う |
| ファイルを添付 (Attach Files) | エージェントがファイルを送る |
| 埋め込みリンク (Embed Links) | 返信内の URL をプレビュー表示する |
| リアクションの追加 (Add Reactions) | 受付の合図 (👀 など)、`add_reaction` ツール |

URL を手で組み立てたくなければ、Developer Portal の **OAuth2 → URL Generator** で **Scopes** に `bot` を選び、**Bot Permissions** で上の権限にチェックを入れても同じ URL が得られる。

管理者 (Administrator) 権限は不要。付けないこと。

> プライベートチャンネルで使うには、そのチャンネルの権限設定で bot (または bot のロール) を追加する必要がある。

### 1-6. ID の調べ方 (許可リストを使う場合)

kojo 側の設定で応答するユーザー / サーバー / チャンネルを絞るには、それぞれの ID が要る。

1. Discord の **ユーザー設定 → 詳細設定 → 開発者モード** をオンにする
2. ユーザー、サーバーのアイコン、チャンネル名を右クリック (モバイルは長押し) して **ID をコピー**

## 2. kojo 側の設定

1. kojo の **設定 → 拡張機能** を開く
2. **リポジトリ URL** に `https://github.com/loppo-llc/kojo-ext-discord` を入れて **内容を確認** を押す
3. 要求される権限 (`chat:session` — 専用セッションでエージェントと会話する) を確認し、**権限を承認して導入** を押す
4. 導入済み一覧の **Discord** を開き、**エージェントごとの有効化** でつなぎたいエージェントを有効にする
5. そのエージェントの設定欄の **Bot token** に 1-2 のトークンを貼り、**設定を保存** を押す

保存するとそのエージェント用のサービスが起動し、数秒で Discord 上の bot がオンラインになる。チャンネルで bot をメンションして話しかければ返事が来る。

トークンは kojo の資格情報ストアに暗号化して保存され、画面にも API にも二度と出てこない。差し替えるときは新しい値を入力して保存、消すときは **削除** を押して保存する。

### 設定項目

| 項目 | 既定 | 説明 |
| --- | --- | --- |
| Bot token | — | 必須。bot のトークン |
| Answer mentions and replies | オン | サーバーのチャンネルでメンション / bot へのリプライに応答する |
| Keep answering in threads | オン | bot が一度発言したスレッドでは、メンションなしでも応答する。Message Content Intent が必要 |
| Answer in a new thread | オフ | メンションされたら、そのメッセージにスレッドを作ってそこで答える。スレッドごとに別の会話になる。オフならチャンネルに直接返信し、チャンネル全体で 1 つの会話になる |
| Answer direct messages | オフ | DM に応答する。**下の注意を読むこと** |
| Use the Message Content intent | オフ | Message Content Intent を使う。Developer Portal 側でも有効にすること (1-4) |
| Channel history as context | 20 | 会話開始時にエージェントへ渡すチャンネルの過去メッセージ数 (0〜100、0 で無効) |
| Allowed users | 空 | 応答するユーザー ID (カンマ区切り)。空なら全員。ID として読めない値しか入っていない場合は誰にも応答しない (他の許可リストも同じ) |
| Allowed servers | 空 | 応答するサーバー ID (カンマ区切り)。空なら bot が参加している全サーバー |
| Allowed channels | 空 | 応答するチャンネル ID (カンマ区切り)。スレッドは親チャンネルに従う。空なら bot に見える全チャンネル |

### 誰が話しかけられるかに注意

Discord 上で bot に話しかけた人は、**そのエージェントに指示を出せる**。エージェントがシェルやファイルを扱えるなら、それも含めて。次を強く勧める:

- bot は信頼できる人しかいないサーバーにだけ入れる。1-3 の Public Bot オフを必ずやる
- 信頼しきれない人がいるサーバーでは **Allowed users** に自分 (と信頼できる人) の ID を入れる
- **Answer direct messages** をオンにするなら、必ず **Allowed users** も設定する。bot と同じサーバーにいる人は誰でも DM を送れる

## 使い方

- **話しかける**: チャンネルで `@bot名 こんにちは` と書く。bot の発言へのリプライでもいい
- **処理中に追記する**: エージェントが考えている間に同じ会話へもう一度書くと、実行中のターンに追加指示として渡る (👀 が付く)。追加指示に対応していないバックエンドでは順番待ちになる
- **止める**: 処理中に `@bot名 !stop` と書く (DM や追従中のスレッドなら `!stop` だけでいい)。🛑 が付いて止まる
- **会話の単位**: チャンネル (またはスレッド、DM) ごとに 1 つのセッション。別のチャンネルの会話は覚えていないが、エージェント自身の記憶ファイルは共有される
- **返信しないこともある**: エージェントが返事不要と判断したときは何も投稿しない

### エージェントが使えるツール

この拡張を有効にしたエージェントには、MCP サーバー `discord_bot` として次のツールが増える。kojo の画面からの指示や cron の中でも使えるので、「毎朝 #news に要約を投稿して」のような使い方ができる。

| ツール | 内容 |
| --- | --- |
| `list_servers` | bot が参加しているサーバーの一覧 |
| `list_channels` | サーバーのテキストチャンネルとアクティブなスレッドの一覧 |
| `read_messages` | チャンネル / スレッドの最近のメッセージを読む |
| `send_message` | メッセージを投稿する。リプライ指定、ファイル添付 (10 個、各 25 MB まで) ができる |
| `add_reaction` | メッセージにリアクションを付ける |
| `create_thread` | スレッドを作る |

ツールにも **Allowed servers / Allowed channels** の制限がかかる。**Allowed users** を設定している場合、`read_messages` と会話開始時の過去ログには許可したユーザーと bot 自身の発言だけが含まれ、DM の読み書きも許可したユーザー相手に限られる。`create_thread` はテキストチャンネルとアナウンスチャンネルでのみ使える。

## うまく動かないとき

| 症状 | 確認すること |
| --- | --- |
| bot がオフラインのまま | Bot token を保存したか。拡張機能とエージェントの両方が有効か。kojo のログに `Discord rejected the bot token` と出ていればトークンが違う — Reset Token してやり直す |
| メンションしても無反応 | bot がそのチャンネルを見られるか (プライベートチャンネルは bot を追加する)。Allowed users / servers / channels に引っかかっていないか。`@bot名` が bot ではなく同名のロールへのメンションになっていないか |
| スレッドで続きを書いても無反応 | Message Content Intent を Developer Portal と kojo の両方でオンにしたか。その bot が一度でも発言したスレッドか |
| 返信は来るが過去ログを踏まえていない | Message Content Intent がオフだと、他人の発言の本文が読めない |
| `Missing Permissions` / `Missing Access` がログに出る | 招待時の権限か、チャンネルごとの権限上書きで bot が拒否されている |
| ⚠️ で始まるメッセージが返る | エージェント側のエラー。文面に理由が出る。kojo のログも見る |

ログは kojo 本体のログに `extension=discord` 付きで出る。

## 制限

- スラッシュコマンド、ボタンでの質問応答、音声チャンネルには未対応
- 返信はターン完了後にまとめて投稿する (処理中は「入力中…」が出る)
- Discord の制限により 1 メッセージは 2000 文字まで。超えた分は自動で分割する。表 (table) は Discord が描画できない
- フォーラムチャンネルの投稿はスレッドとして扱う

## 開発

```sh
make test    # go vet + go test
make build   # bin/ 以下に各プラットフォームのバイナリを作る
```

kojo は拡張パッケージを `git clone` して導入するだけでビルドはしない。そのためバイナリはリポジトリにコミットしてある。コードを変えたら `make build` して `bin/` も一緒にコミットし、`kojo-package.json` の `version` を上げること。

同じバイナリが 2 つの役をする: 引数なしなら Gateway に常駐するサービス、`mcp` を付けると stdio の MCP サーバー。

## ライセンス

MIT
