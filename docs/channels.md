# channels

Read when: listing, joining, leaving, muting, reading, reacting to, creating, or sending to WhatsApp Channels.

`wacli channels` manages WhatsApp Channels, which `whatsmeow` calls newsletters. Commands use live WhatsApp APIs and require authentication and writable mode, since they open the WhatsApp session.

## Commands

```bash
wacli channels list
wacli channels info --jid CHANNEL_JID
wacli channels join --invite LINK_OR_CODE
wacli channels leave --jid CHANNEL_JID
wacli channels mute --jid CHANNEL_JID
wacli channels unmute --jid CHANNEL_JID
wacli channels messages --jid CHANNEL_JID [--count N] [--before SERVER_ID]
wacli channels react --jid CHANNEL_JID --server-id ID --reaction EMOJI
wacli channels mark-viewed --jid CHANNEL_JID --server-id ID [--server-id ID...]
wacli channels create --name NAME [--description TEXT]
```

## Notes

- Channel JIDs use the `...@newsletter` server.
- `channels list` fetches subscribed channels live and updates local chat rows with kind `newsletter`.
- `channels info` fetches one joined channel live and updates the local chat row.
- `channels join` accepts a full `https://whatsapp.com/channel/...` link or just the invite code.
- `channels leave` unfollows the channel on WhatsApp.
- `channels mute` and `channels unmute` change notifications for a followed channel on this account. `channels info` shows the current state in `mute`.
- `channels messages` fetches recent posts live, newest first, with each post's `server_id`, message `id`, time, type, view count, reaction counts, text, and a media summary (type, caption, filename, MIME type). `--count` defaults to 20; `--before SERVER_ID` pages back to older posts. Nothing is written to the local store: a post's server ID, views, and reactions have no place in the messages table, and new posts of followed channels already arrive through sync.
- `channels react` reacts to a post by the server ID that `channels messages` prints. `--reaction ''` removes this account's reaction. The channel's reaction counts include it.
- `channels mark-viewed` counts posts as viewed, which raises their view counters. It is not the same as marking the channel read on your other devices. `--server-id` can be repeated or comma-separated.
- `channels create` creates a channel owned by this account and stores it as a chat. Channels are public, and wacli cannot delete one. WhatsApp may first require accepting its channel terms in the phone app.
- `sync --refresh-channels` refreshes subscribed channel names into the local chat cache.
- `send text --to ...@newsletter` can send to channels when the authenticated account has permission.
- `send file --to ...@newsletter` uses WhatsApp's unencrypted newsletter media upload path and requires channel posting permission.
- Quoted file replies and `--ptt` voice-note mode are not supported for channels.

## Running beside sync

Every `channels` command needs the WhatsApp connection. While `sync --follow` holds the store, they run inside it over the local delegate socket and print the same output, with or without `--json`. `--read-only` and `WACLI_READONLY` are checked first. See [sync](sync.md).

## Examples

```bash
wacli channels list
wacli channels info --jid 120363000000000001@newsletter
wacli channels join --invite https://whatsapp.com/channel/AbCdEfGhIjK
wacli channels leave --jid 120363000000000001@newsletter
wacli channels mute --jid 120363000000000001@newsletter
wacli channels messages --jid 120363000000000001@newsletter --count 10 --json
wacli channels messages --jid 120363000000000001@newsletter --before 120
wacli channels react --jid 120363000000000001@newsletter --server-id 118 --reaction "👍"
wacli channels react --jid 120363000000000001@newsletter --server-id 118 --reaction ''
wacli channels mark-viewed --jid 120363000000000001@newsletter --server-id 117,118
wacli channels create --name "Test channel" --description "Updates"
wacli send text --to 120363000000000001@newsletter --message "Hello channel"
wacli send file --to 120363000000000001@newsletter --file ./image.png --caption "Update"
```
