# messages

Read when: listing, searching, exporting, showing, inspecting local message context, or mutating stored messages.

Most `wacli messages` commands read from the local store. `messages edit`, `messages delete`, `messages revoke`, `messages forward`, `messages star`/`unstar`, `messages pin`/`unpin`, and `messages keep`/`unkeep` are remote WhatsApp mutations and require an authenticated, writable store.
WhatsApp status broadcasts are stored separately in `status_messages`; they are not returned by `messages list`, `messages search`, or `messages export`.

## Commands

```bash
wacli messages list [--chat JID] [--sender JID] [--from-me|--from-them] [--asc] [--limit N] [--after DATE] [--before DATE] [--forwarded] [--starred]
wacli messages search <query> [--chat JID] [--from JID] [--has-media] [--type text|image|video|audio|document] [--forwarded] [--starred] [--limit N] [--after DATE] [--before DATE]
wacli messages starred [--chat JID] [--limit N] [--after DATE] [--before DATE] [--asc]
wacli messages export [--chat JID] [--limit N] [--after DATE] [--before DATE] [--output PATH]
wacli messages show --chat JID --id MSG_ID
wacli messages context --chat JID --id MSG_ID [--before N] [--after N]
wacli messages edit --chat JID --id MSG_ID --message TEXT [--post-send-wait 2s]
wacli messages delete --chat JID --id MSG_ID [--for-me] [--delete-media] [--post-send-wait 2s]
wacli messages purge --chat JID --id MSG_ID [--dry-run] [--confirm]
wacli messages revoke --chat JID --id MSG_ID [--post-send-wait 2s]
wacli messages forward --chat JID --id MSG_ID --to RECIPIENT [--pick N] [--post-send-wait 2s]
wacli messages star --chat JID --id MSG_ID
wacli messages unstar --chat JID --id MSG_ID
wacli messages pin --chat JID --id MSG_ID [--duration 24h|7d|30d] [--post-send-wait 2s]
wacli messages unpin --chat JID --id MSG_ID [--post-send-wait 2s]
wacli messages keep --chat JID --id MSG_ID [--post-send-wait 2s]
wacli messages unkeep --chat JID --id MSG_ID [--post-send-wait 2s]
wacli messages pinned [--chat JID]
```

## Search

- Uses SQLite FTS5 when the binary was built with `-tags sqlite_fts5`.
- Falls back to `LIKE` if FTS5 is not available.
- `--type` accepts `text`, `image`, `video`, `audio`, or `document`.
- Shared WhatsApp contact cards are stored as searchable text with contact names and phone numbers when WhatsApp includes a vCard payload.
- Associated-child and group-status-mention wrappers retain their inner text, media, and reply context. Group invitations expose their caption, with the group name as a fallback. These parser fixes apply on re-ingestion; they cannot recover absent payloads or missing decryption keys.
- Comment payloads retain their inner text or media and their envelope's reply target. Album headers show expected image/video counts; those summaries do not recover missing child captions or undecryptable history.
- `--starred` restricts list/search results to messages marked as starred by WhatsApp.
- Time filters accept RFC3339 or `YYYY-MM-DD`.

## Media captions

Plain audio messages have an empty `MediaCaption`. Their `Text` keeps the `[Audio]` display fallback, so they can still match searches for `Audio`. Text supplied alongside an audio payload remains its caption, including a literal `[Audio]` supplied by the sender. Existing rows are not migrated; an ordinary live or history re-ingestion can replace a legacy synthetic caption, subject to the existing edit and deletion rules.

## Voice note transcripts

`wacli media transcribe` stores speech-to-text for audio messages in a sidecar `transcripts.db` (see [media](media.md#transcribe)). When an audio message has a transcript:

- `messages list`, `show`, `context`, `search`, `starred`, and `export` add `transcript` (the text) and `transcript_engine` (for example `fluidaudio`) to the message JSON. Both fields are omitted when there is no transcript. `"transcript": ""` means the engine heard no speech.
- Human tables show `[Voice] <transcript>` in place of the `Sent audio` placeholder (a reply keeps its quoted line), and `messages show` adds `Transcribed by: <engine>`.
- `Text`, `DisplayText`, and every other stored field are unchanged. A transcript is never written into typed-text columns.
- If `transcripts.db` does not exist, output is exactly as before and read commands do not create it.

`messages search <query>` also matches transcripts:

- A transcript matches when it contains every word of the query, compared case-insensitively (ASCII case folding, like the `LIKE` fallback). Wildcards are literal.
- Transcript matches honor the same filters as regular results: `--chat` (including mapped `@lid` rows), `--from`, `--after`/`--before`, `--has-media`, `--forwarded`, `--starred`, and `--type`. Any `--type` other than `audio` excludes them.
- Their `Snippet` looks like the FTS snippet with a `[Voice]` prefix, for example `[Voice] bring the [picnic] blanket`.
- A message found by both its own text and its transcript appears once.
- When transcript matches are added, the combined list is ordered newest first and cut to `--limit`. FTS relevance scores and transcript matches cannot be compared, so time is the one shared order. When no transcript matches, results keep their usual order (FTS rank, or newest first for `LIKE`).

## Starred

- `messages starred` lists starred messages ordered by star time when app-state events provide it; history-imported rows fall back to message time.
- `--after` and `--before` on `messages starred` filter by that stored star time.
- Starred state is imported from history sync and app-state star/unstar events.
- `messages star` and `messages unstar` change the star on all your devices with WhatsApp's `star` app-state patch (`regular_high`) and update the local starred state that `messages starred` reads. The message must be stored locally and not deleted. For a group message someone else sent, the stored sender is required; in a LID-addressed group wacli names the sender by LID, as WhatsApp does.

## Pin and keep

- `messages pin` pins a stored message for everyone in the chat; `--duration` is `24h`, `7d` (default) or `30d`, the choices WhatsApp offers. `messages unpin` removes the pin. WhatsApp may refuse pins in groups where only admins can pin.
- `messages pinned` lists current pins from the local store, newest first, with the pinned message when it is stored. Pins made on other devices arrive during sync and are recorded too; a pin without a known duration stays listed until it is unpinned.
- `messages keep` keeps a message from disappearing in a chat with disappearing messages on; `messages unkeep` undoes it. WhatsApp ignores keep in chats without a timer; wacli cannot check the timer locally. See `chats disappearing`.
- Pin and keep are protocol messages the other participants receive. Their notices are not stored as unread messages.

## Export

- `messages export` writes a JSON export envelope with messages ordered oldest first.
- Use `--chat` to export one chat, or omit it to export recent messages across chats.
- Use `--after` and `--before` to bound the exported time window.
- Use `--output` to write the JSON export to a file.

## Edit and Delete

- `messages edit` updates one of your own recent sent text messages. WhatsApp only accepts edits inside its current edit window.
- `messages delete` revokes one of your own sent messages for everyone. `messages revoke` is the explicit form of the same delete-for-everyone operation.
- `messages delete --for-me` removes a stored message only for your WhatsApp account using WhatsApp's `deleteMessageForMe` app-state patch; it can target messages sent by you or by others. `--delete-media` is only valid with `--for-me`.
- `messages forward` forwards a stored text, image, video, GIF, audio, sticker, or document message to another recipient and marks the outgoing copy as forwarded. Media forwards require synced media metadata; reaction forwarding is not supported.
- These commands look up the target in the local store first and honor `--read-only`/`WACLI_READONLY`. Delete-for-everyone and edit require a message sent by you.
- While a same-store `sync --follow` owns the store lock, `edit`, `delete` (including `--for-me` and `--delete-media`), `revoke`, `forward`, `star`, `unstar`, `pin`, `unpin`, `keep`, and `unkeep` are delegated to it and print the same output as a direct run. The follow process cannot prompt, so an ambiguous `forward --to` name needs `--pick N`, as with `--json`. Restart an older sync process after upgrading; it rejects a command it predates without running it.
- Deleted messages, WhatsApp delete-for-me events, and messages removed by deleting or clearing a whole chat (`chats delete`/`chats clear`, or the same on another device; `deletion_reason` `whatsapp-delete-chat` or `whatsapp-clear-chat`) are kept as local tombstones with `deleted_at` and `deletion_reason`. Their original text, reply, interactive, and media metadata remains available to direct `messages show`, but tombstones stay hidden from normal list/search/starred/export results and FTS.
- Sync, history, and backfill ingestion merge messages by chat JID and message ID. A message missing from any partial import is left unchanged, and a later live copy does not resurrect an existing tombstone.
- `messages purge` is the deliberate payload-erasure path. It only accepts an already tombstoned row, removes downloaded local media, clears its retained `wacli.db` payload, and requires confirmation unless `--confirm` is passed. A minimal tombstone with `payload_purged_at` and a non-cascading purge-ledger key remain so later sync or history imports cannot restore the payload after chat cleanup.

## LID mapping

When a phone-number chat JID maps to a stored `@lid` row, list/search/show/context include the mapped rows so historical LID splits do not hide messages.

## Examples

```bash
wacli messages list --chat 1234567890@s.whatsapp.net --asc
wacli messages list --from-me --limit 20
wacli messages starred --limit 20
wacli messages search "invoice" --has-media --type document
wacli messages search "invoice" --starred
wacli messages export --chat 1234567890@s.whatsapp.net --after 2024-01-01 --before 2024-02-01 --output messages.json
wacli messages show --chat 1234567890@s.whatsapp.net --id ABC123
wacli messages context --chat 1234567890@s.whatsapp.net --id ABC123 --before 3 --after 3
wacli messages edit --chat 1234567890@s.whatsapp.net --id ABC123 --message "updated text"
wacli messages delete --chat 1234567890@s.whatsapp.net --id ABC123
wacli messages delete --chat 1234567890@s.whatsapp.net --id ABC123 --for-me
wacli messages purge --chat 1234567890@s.whatsapp.net --id ABC123 --dry-run
wacli messages revoke --chat 1234567890@s.whatsapp.net --id ABC123
wacli messages forward --chat 1234567890@s.whatsapp.net --id ABC123 --to "Family"
```
