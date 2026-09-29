# chats

Read when: listing known chats, filtering chat state, archiving/pinning/muting/marking chats, deleting/clearing/locking chats, setting disappearing messages, using WhatsApp lists and favorites, or pruning stale local chat rows.

`wacli chats` reads chat rows from `wacli.db`. It can use session-backed PN/LID mappings to make historical `@lid` chat rows display as phone-number chats when possible. State commands normally send WhatsApp app-state patches through the authenticated session and update the local index after WhatsApp accepts the change. Explicit receipt mode uses the independent network receipt path described below.

## Commands

```bash
wacli chats list [--query TEXT] [--limit N] [--archived|--no-archived] [--pinned|--no-pinned] [--muted|--no-muted] [--unread|--no-unread] [--locked|--no-locked] [--deleted] [--list LIST]
wacli chats show --jid JID
wacli chats archive --chat CHAT [--pick N]
wacli chats unarchive --chat CHAT [--pick N]
wacli chats pin --chat CHAT [--pick N]
wacli chats unpin --chat CHAT [--pick N]
wacli chats mute --chat CHAT [--duration DURATION] [--pick N]
wacli chats unmute --chat CHAT [--pick N]
wacli chats mark-read --chat CHAT [--pick N] [--receipts]
wacli chats mark-unread --chat CHAT [--pick N]
wacli chats cleanup [--days N] [--jid JID] [--dry-run] [--confirm]
wacli chats delete --chat CHAT [--pick N] [--delete-media] [--confirm]
wacli chats clear --chat CHAT [--pick N] [--delete-starred] [--delete-media] [--confirm]
wacli chats lock --chat CHAT [--pick N]
wacli chats unlock --chat CHAT [--pick N]
wacli chats disappearing --chat CHAT --duration off|24h|7d|90d [--pick N]
wacli chats favorite --chat CHAT [--pick N]
wacli chats unfavorite --chat CHAT [--pick N]
wacli chats lists [--include-deleted]
wacli chats lists create NAME
wacli chats lists rename LIST NEW_NAME
wacli chats lists delete LIST [--confirm]
wacli chats lists add --list LIST --chat CHAT [--pick N]
wacli chats lists remove --list LIST --chat CHAT [--pick N]
```

## Notes

- `list` is local and sorted by pinned chats first, then newest known message timestamp.
- WhatsApp system events, such as a changed security code or a group notice, arrive as payloads with no content and are stored as `(message)` rows. They do not set that timestamp, so they cannot move a chat up the list; a chat that holds nothing else has no timestamp and sorts last.
- On the next writable open, activity matching the newest locally stored message is recomputed from stored content. Activity newer than every local message is preserved. If an unstored message advertised by history has the exact same second as a local placeholder, the repair uses local content; syncing that message restores its activity.
- `--query` filters by chat name or JID.
- `list --json` and `show --json` include `archived`, `pinned`, `muted_until`, `unread`, `unread_count`, and `locked`, plus `deleted_at`/`cleared_at` when set; `unread` is true for counted unread messages and marker-only unread chats, while `unread_count` only counts unread messages.
- `list --unread` matches counted and marker-only unread chats; `list --no-unread` excludes both.
- Replayed read signals reduce the unread count only through the messages they cover; older reads cannot restore already-read messages. Known message IDs distinguish arrivals in the same second, using their local insertion order. Without a known ID boundary, messages at the cutoff second remain unread. Content-free system events, reactions, and revocations do not add to live unread counts.
- `mark-unread` sets the unread marker without inventing an unread count; `mark-read` clears the marker and count through its captured message boundary; arrivals beyond it remain unread.
- `show` accepts the stored JID. If a phone JID maps to a historical `@lid` row, it can show that row too.
- State commands use `--chat` and resolve names, phone numbers, groups, and JIDs like send commands. Use `--pick N` for ambiguous matches.
- After a same-store `sync --follow` process finishes startup and opens its local delegate socket, every state command (`archive`/`unarchive`, `pin`/`unpin`, `mute`/`unmute`, `mark-read`/`mark-unread`, `delete`, `clear`, `lock`/`unlock`, `disappearing`, `favorite`/`unfavorite`, and the `lists` changes) is delegated to it while it owns the store lock, with the same output as a direct run. The follow process cannot prompt, so an ambiguous `--chat` name needs `--pick N` there, as with `--json`.
- Restart an older `sync --follow` process after upgrading before using delegated state commands. An older process rejects a command it predates, reporting an unsupported kind such as `chat_archive` or `mark_read`, without changing the chat.
- State commands print a compact success line by default and a stable JSON object with `--json`.
- `mute --duration 0` or omitting `--duration` mutes forever. Use `unmute` to clear it.
- Run `wacli sync` to catch up chat-state changes made on other devices; run `wacli contacts refresh` to improve chat names.
- `cleanup` only deletes local `wacli.db` rows. It does not delete chats or messages from WhatsApp.
- `cleanup --days N` skips chats with no known local activity timestamp; use `--jid` for an explicit local row.
- Use `cleanup --dry-run` before deleting and `--confirm` only for scripts that already reviewed the target list.

## Delete and clear

`chats delete` deletes a chat on all your devices, as the phone's Delete chat does; `chats clear` removes its messages but keeps the chat. Both send WhatsApp's `deleteChat`/`clearChat` app-state patch (`regular_high`) covering every message up to now, including ones this store never saw. They change WhatsApp on every device, so they ask for confirmation, or need `--confirm` in scripts and with `--json`.

wacli keeps its own copy as tombstones, the same way it keeps deleted messages: messages up to the delete or clear point get `deleted_at` and `deletion_reason` (`whatsapp-delete-chat` or `whatsapp-clear-chat`), leave `messages list`, `search`, `starred`, `export` and FTS, and stay readable with `messages show`. `messages purge` can then erase a payload deliberately. Nothing is hard-deleted, and downloaded media files stay on disk; `--delete-media` only asks your devices to delete the chat's media.

- A deleted chat is hidden from `chats list` until a message newer than the delete arrives, as on the phone. `chats list --deleted` shows deleted chats; `chats show` still shows one, with `deleted_at`.
- `clear` keeps starred messages unless `--delete-starred` is passed, matching the phone's default. A cleared chat stays listed with `cleared_at`.
- Deleting or clearing also resets the chat's local unread count.
- Deletes and clears made on other devices are mirrored the same way during sync, using the message range the phone sent. When another device cleared starred messages too, wacli tombstones them as well.
- A full app-state replay of `regular_high` (after an LTHash mismatch, or the first `chats favorite` after upgrading) re-applies deletes and clears made before wacli mirrored them, so chats you deleted on the phone long ago can disappear from `chats list` then; their messages remain tombstones.
- WhatsApp's apps only offer Delete for a group after you exit it; leave with `wacli groups leave` first. Deleting a group you are still in may be ignored by the phone.

## Lock and disappearing messages

- `chats lock`/`chats unlock` use WhatsApp chat lock (`lock` app-state patch, `regular_low`). A locked chat is hidden in the phone's Locked chats folder; wacli does not hide it locally. `list --locked`/`--no-locked` filter on it, and `list` shows a `locked` flag.
- When the phone locked a chat, unlock writes the same app-state entry (the JID form the phone used), so both agree. Locks set before this feature appear after the next full `regular_low` replay.
- `chats disappearing --duration off|24h|7d|90d` sets the chat's disappearing-message timer with whatsmeow. In one-to-one chats WhatsApp delivers this as a message the other person sees; in groups it is a group setting that admins may restrict.

## Lists and favorites

WhatsApp Lists (the chat filters above the chat list: Unread, Favorites, Groups, and your own lists) are labels at the protocol level. Sync stores them from app state: definitions from `label_edit`, chat membership from `label_jid` (latest state per chat, so a removal wins), and favorites from the separate `favorites` value. The `regular` collection holding lists is re-read in full at every sync start.

- `chats lists` shows lists with their type (`custom`, `favorites`, `unread`, `groups`, ...), order, and chats. Unread, Groups and similar filters are computed by WhatsApp on the phone and show no chats. `--include-deleted` adds deleted lists. It reads the local store and works while `sync --follow` runs.
- `chats list --list LIST` shows the chats in one list, by ID or name (case-insensitive). `--list favorites` always means favorites.
- `chats lists create NAME` makes a custom list. It gets an ID no stored list has used and goes after the other lists. Until a full `regular` sync has been stored, the first list change replays that collection first, so a new ID cannot reuse one the phone made earlier.
- `chats lists rename LIST NEW_NAME` and `chats lists delete LIST` change only custom lists and keep every other field the phone set. Deleting a list does not change its chats.
- `chats lists add|remove --list LIST --chat CHAT` change membership. Removal writes every app-state entry the phone used for that chat (phone number and LID forms).
- `chats favorite`/`chats unfavorite` edit favorites. Favorites travel as one ordered list, so each change writes the whole list; before the first change wacli replays `regular_high` in full unless it already stored the complete list, so existing favorites are kept.
- Message labels (`label_message`) are not stored.

## Read receipts

Use `wacli chats mark-read --chat CHAT --receipts` to send receipts for stored unread incoming messages. Plain `mark-read` keeps its existing app-state behavior and does not notify message senders. `mark-unread` remains available through a running sync process.

Receipt mode does not wait for app-state recovery, so it can run while the primary phone cannot provide an app-state snapshot. It works directly or through a same-store `sync --follow` process. Restart older sync processes after upgrading: the separate receipt request is rejected by an older daemon before it changes the chat.

Each call handles at most 100 messages, starting with the oldest messages in the current unread selection. Repeat the command for larger counts; unsent messages and arrivals during dispatch remain unread. Reactions, outgoing messages, content-free placeholders, and deleted messages are excluded before the limit. Group messages are batched by sender. Missing group sender identities or an unread count larger than the eligible local history fail before dispatch; sync the missing history first. A marker-only chat uses its latest eligible incoming message, or clears only the local marker when no such message is stored.

WhatsApp's read-receipt privacy setting is respected and never changed. JSON output adds `receipts`, the number of dispatched messages. When nonzero it also includes `receipt` and `sender_notified`:

- `receipt: "read-self"`, `sender_notified: false`: explicitly sent only to the account's own devices, including when receipts are disabled.
- `receipt: "unknown"`, `sender_notified: null`: normal receipts were requested, but the transport may apply a newer privacy setting and does not report the emitted type. This is not confirmation that a sender saw blue ticks. Mixed group-batch outcomes are also reported as unknown.

The local read boundary advances after every selected batch has dispatched successfully. If a later batch fails, the error reports how many messages were already dispatched; retrying may resend those receipts. Receipt mode does not send an additional chat-state patch. `--read-only` rejects it because sending receipts changes WhatsApp state.

## Examples

```bash
wacli chats list
wacli chats list --query family --limit 20
wacli chats list --pinned
wacli chats show --jid 1234567890@s.whatsapp.net
wacli chats mute --chat "+1 555 123 4567" --duration 8h
wacli chats mark-read --chat family --pick 1
wacli chats mark-read --chat family --pick 1 --receipts --json
wacli chats cleanup --days 365 --dry-run
wacli chats clear --chat "Family" --confirm
wacli chats disappearing --chat "Family" --duration 7d
wacli chats lists create "Work"
wacli chats lists add --list Work --chat "+1 202 555 0142"
wacli chats list --list Work
```
