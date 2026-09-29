# calls

Read when: listing WhatsApp call events captured by sync, or declining a ringing call.

`wacli calls list` reads call metadata from the local store. `wacli calls reject` declines a call that is still ringing. wacli does not place or accept calls.

## Commands

```bash
wacli calls list [--chat JID] [--asc] [--limit N] [--after DATE] [--before DATE]
wacli calls reject --from JID|PHONE --call-id ID
```

## Data Model

- Live WhatsApp call signaling events are stored separately from normal messages.
- Historical WhatsApp call-log messages are stored as normal message rows and as structured `call_events` rows.
- JSON output includes `chat_jid`, `call_id`, `event_type`, `direction`, `media`, `outcome`, `duration_secs`, `timestamp`, and participant metadata when WhatsApp provides it.
- Time filters accept RFC3339 or `YYYY-MM-DD`.

## Rejecting calls

- `calls reject` sends WhatsApp's call rejection for one call. The caller sees the call declined.
- Take `--from` and `--call-id` from the call's `offer` event in `wacli calls list --json` (`sender_jid` and `call_id`). `--from` accepts a phone number or user JID.
- It only has an effect while the call is still ringing; WhatsApp ignores a rejection for a call that has ended. wacli does not check that the call exists.
- It needs the WhatsApp connection and writable mode. `sync --follow` is the process that sees incoming calls; while it holds the store, `calls reject` runs inside it over the local delegate socket. See [sync](sync.md).
- JSON output is `{"rejected": true, "from": ..., "call_id": ...}`.

## Examples

```bash
wacli calls list --limit 20
wacli calls list --chat 1234567890@s.whatsapp.net --json
wacli calls list --after 2026-05-01 --asc
wacli calls reject --from 15550000001@s.whatsapp.net --call-id FAKECALLID01
```
