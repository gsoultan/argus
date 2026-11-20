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
