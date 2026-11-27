# Telegram bots per organization

Code: internal/control/telegram_bots.go (store, sealing, resolution), telegram_routes.go (HTTP), migration 100_telegram_bots.sql. Console: web/src/components/TelegramBots.tsx (Settings > Telegram bots, admins/owners), web/src/lib/telegram.ts.

- `telegram_bots` (per tenant schema): name, @username, token sealed with AES-GCM under a key derived from signing_secret (grantKeyFor(ctx), "argus telegram tokens v1"), token_tail, chats JSONB [{id,title,kind}], is_default (at most one), last_ok_at / last_error.
- `telegram_routes`: kind in signin | server | website | backup; bot '' default, 'off', 'platform', or a bot id; chats narrow the bot's chats (empty = all).
- A bot only ever posts to its own listed chats (notify.Telegram.Allowed).
- Default bot resolution: the org's default bot, else the platform bot (config file) only for the default organization. Other orgs never use the platform bot.
- Backups: the platform's Dispatcher already sends them to the platform channels, so the backup route never resolves to the platform bot.
- Sign-in and server alerts: notify.SignInNotifier.Admins/Reported/ServerConnected take a `tg func(Rendered)`; control passes routedTelegram so Telegram follows the route. With no platform notifier, an org's own bots still get them.
- Sends are async (API.queueTelegram, 16 in flight) and record last_ok/last_error on the bot.
- API: GET /api/v1/telegram; POST/PUT/DELETE(?moveTo=) /api/v1/telegram/bots[/{id}]; POST /bots/{id}/test; POST /check {token}; POST /chats {token|bot} (getUpdates); PUT /routes/{kind}; POST /routes/{kind}/test. All audited (telegram.bot.add|change|remove, telegram.route).
- Tests: TestTelegramBotsAndRoutes, TestTelegramRoutesHTTP (fake Bot API per token), e2e/telegram-bots.spec.ts.

## Getting the token and a group's chat ID (2026-11)

Most people could not work out a group's chat ID, so both Telegram forms
(the bot form in Settings › Notifications and the platform bot in Settings ›
Alert channels) carry the steps from web/src/components/TelegramHelp.tsx:
TokenHelp (@BotFather, /newbot, /revoke) and ChatHelp (add the bot, send
`/start@<bot>` — privacy mode hides plain messages from bots in groups — then
Find chats; negative IDs, -100 for supergroups/channels; channels, private
chats, webhooks, upgrades, getUpdates by hand).

- useChatFinder: Find chats looks once; when nothing new turns up it watches
  (every 3 s for 2 min) and ticks the first new non-private chat that appears.
- notify.Telegram.RecentChats leaves out chats the bot was removed from and a
  group's old ID once it became a supergroup (migrate_to/from_chat_id), most
  recently heard first. Test: internal/notify/telegram_chats_test.go.
- Platform bot helpers: POST /api/v1/settings/telegram-chats {botToken?} and
  /api/v1/settings/test-telegram {botToken?, chatIds}; admins/owners of the
  default organization only (404 elsewhere); a token left out is the running
  one. Test: TestPlatformTelegramHelpers.
