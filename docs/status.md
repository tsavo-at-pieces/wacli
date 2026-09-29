# status

Read when: reading synced status updates (stories), downloading their media, muting a contact's status updates, or checking who sees your status.

`wacli status` works with WhatsApp status updates. Sync stores them in the local `status_messages` table, separate from chat messages. To post a status, use [`send status`](send.md).

## Commands

```bash
wacli status list [--from JID] [--after DATE] [--before DATE] [--limit N]
wacli status show --id ID
wacli status download --id ID --output PATH
wacli status mutes [--all]
wacli status mute --jid JID|PHONE|NAME [--pick N]
wacli status unmute --jid JID|PHONE|NAME [--pick N]
wacli status privacy
```

## Local reads

- `list`, `show`, `download`, and `mutes` read the local store. They take no store lock, so they run while `sync --follow` is running, and they work in `--read-only` mode.
- `list` prints the newest statuses first. `--from` accepts a phone number or JID and matches both the phone and LID identity of that contact. `--after` and `--before` accept RFC3339 or `YYYY-MM-DD`.
- JSON records include `id`, `timestamp`, `from_me`, `sender_jid`, `sender_name`, `sender_muted`, `text`, the media type, caption, filename, MIME type and size, `downloadable`, and for your own text statuses `background_color` and `font`. Media keys and direct paths stay in the store.
- `sender_muted` is true when the sender's status updates are muted in WhatsApp, as mirrored by sync (see below).
- `download` fetches and decrypts the media straight from WhatsApp's media servers, the same way as `media download --read-only`. It needs no WhatsApp connection and does not record anything in the store. `--output` is required; when it names an existing directory, the file is named after the stored filename or the status ID. Status media disappears from WhatsApp's servers after about a day, so download soon after sync stores the status.

## Mutes

- `status mute` and `status unmute` change whether this account sees a contact's status updates. The contact is not told. The change syncs to your other devices through WhatsApp app state (`userStatusMute` in the `regular_high` collection).
- `--jid` accepts a JID, a phone number, or a synced contact name; `--pick N` selects a match when a name is ambiguous.
- Sync mirrors every status mute or unmute made on any device into the local `status_mutes` table. When a mute for the contact is already mirrored, `mute` and `unmute` write to the same identity (phone JID or LID) that WhatsApp used, so an unmute reverses a mute made on the phone.
- `status mutes` lists muted contacts; `--all` includes unmuted entries. Mutes made before this store existed appear only after WhatsApp resends the full app state, for example after pairing again.
- JSON output is `{"jid": ..., "muted": true|false}` with the JID written to app state.

## Privacy

`status privacy` fetches this account's status audience lists live from WhatsApp: `contacts` (all contacts), `blacklist` (all contacts except the listed ones), or `whitelist` (only the listed ones). The default list applies to new status updates. It needs a WhatsApp connection and writable mode.

## Running beside sync

`status mute`, `status unmute`, `status privacy`, and `send status` need the WhatsApp connection. While `sync --follow` holds the store, they run inside it over the local delegate socket and print the same output. `--read-only` and `WACLI_READONLY` are checked first. See [sync](sync.md).

## Examples

```bash
wacli status list --limit 20
wacli status list --from 15550000001 --after 2026-05-01 --json
wacli status show --id 3EB0FAKESTATUS01
wacli status download --id 3EB0FAKESTATUS01 --output ~/Downloads/
wacli status mute --jid 15550000001@s.whatsapp.net
wacli status unmute --jid "Sam" --pick 1
wacli status mutes --json
wacli status privacy --json
```
