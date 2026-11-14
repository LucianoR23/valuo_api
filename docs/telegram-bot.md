# Telegram bot (plan 3.6)

People link their Telegram chat from the app (Ajustes › Telegram) and then send
expenses to the bot as text, voice notes (up to 30 s) or photos of tickets and
payment screenshots (up to 5 MB). Each expense is shown back with ✅ ✏️ ❌ and
saved only on ✅; for a voice note or photo the card also says what was heard
or read. Commands: `/resumen`, `/comprometido`, `/deshacer`, `/ayuda`.

- Only linked private chats are served. Any other chat gets one fixed reply
  (at most every 10 minutes) and never reaches the AI (plan §8.1).
- The webhook checks `X-Telegram-Bot-Api-Secret-Token`; everything else gets 401.
- Each chat may send 20 updates a minute; more are dropped.
- AI calls go through the same parser as the app and are recorded in `ai_usage`
  with channel `telegram`, under the same limits.
- Automatic AI suspension (plan §8.6): 5 answers in a day that are not
  expenses, or a day's AI cost over 10 times what the other accounts spend on
  an average day (and over US$ 0.01), turn the AI off for that account for
  24 h. Superadmins get an email; `/admin/ia` lists suspended accounts and
  lifts it (`POST /admin/ai/users/{id}/resume`, audited). Typed amounts keep
  working through the rules.
- Voice notes and photos are downloaded into memory for the call and never
  stored (plan §8b).
- Nothing from the messages is stored except the draft being confirmed
  (`telegram_drafts`, deleted after 24 h by the hourly `telegram_cleanup` job).

Messages transit Telegram's servers and bot chats are not end-to-end
encrypted. The app shows this before linking (plan §13).

## Setting up staging (once)

1. **Create the bot.** In Telegram, talk to @BotFather: `/newbot`, a name
   (e.g. "Valuo staging") and a username ending in `bot`. Keep the token in
   your password manager. Optional: `/setdescription`, `/setuserpic`.
   Leave privacy mode on and do not add the bot to groups (it ignores them).
2. **Secret.** `openssl rand -hex 32`, also in the password manager.
3. **Coolify, staging API app, Environment Variables:**
   `BOT_TELEGRAM_ENABLED=true`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_WEBHOOK_SECRET`,
   `TELEGRAM_BOT_USERNAME` (without `@`). `API_PUBLIC_URL` must be the public
   https origin (`https://api-next.valuo.ar`). Redeploy: migration 000049 runs
   and the API registers the webhook and the command menu on start (the log
   says `telegram: webhook registered`).
4. **Check.** Open the app, Ajustes › Telegram › Conectar, accept, and press
   Start in Telegram. Send "café 2500 efectivo", tap ✅ and look for the
   expense in the app. To see what Telegram thinks of the webhook:
   `https://api.telegram.org/bot<token>/getWebhookInfo` (paste it in the
   browser; never in a shared place, it has the token).

Production uses its own bot and secret, set the same way.
