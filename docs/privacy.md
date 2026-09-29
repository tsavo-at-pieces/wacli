# privacy

Read when: reading or changing the account's WhatsApp privacy settings, the default disappearing-message timer, or who status updates go to.

`wacli privacy` reads and changes account-level privacy settings for the linked account. Changes apply to the whole account, on the phone and every linked device.

## Commands

```bash
wacli privacy show
wacli privacy set <setting> <value>
wacli privacy disappearing-default --duration 0|24h|7d|90d
wacli privacy status
```

## Settings

| Setting | Wire name | Values |
| --- | --- | --- |
| `last-seen` | `last` | `all`, `contacts`, `contact_blacklist`, `none` |
| `online` | `online` | `all`, `match_last_seen` |
| `profile-photo` | `profile` | `all`, `contacts`, `contact_blacklist`, `none` |
| `about` | `status` | `all`, `contacts`, `contact_blacklist`, `none` |
| `read-receipts` | `readreceipts` | `all`, `none` |
| `group-add` | `groupadd` | `all`, `contacts`, `contact_blacklist` |
| `call-add` | `calladd` | `all`, `known` |
| `messages` | `messages` | `all`, `contacts` |
| `defense` | `defense` | `on_standard`, `off` |
| `stickers` | `stickers` | `contacts`, `contact_allowlist`, `none` |

- `all` is "Everyone", `contacts` is "My contacts", `contact_blacklist` is "My contacts except...", `none` is "Nobody". `online` `match_last_seen` is "Same as last seen"; `call-add` `known` is the phone's "Silence unknown callers".
- `set` accepts the wacli name, the wire name, or either with underscores (`last_seen`), in any case. Values are checked per setting before anything is sent.
- `contact_blacklist` and `contact_allowlist` use the exception list already set on the phone. wacli cannot edit that list.

## Notes

- `show` fetches the current settings from WhatsApp, bypassing whatsmeow's cache. JSON keys are the setting names with underscores (`last_seen`, `profile_photo`, `read_receipts`, `group_add`, `call_add`, ...). A setting WhatsApp did not report is an empty string (`-` in the table).
- `set` reads the current value first and reports it as `previous`, so a change can be undone with `privacy set <setting> <previous>`.
- `disappearing-default` sets the timer new one-to-one chats start with. Existing chats keep their own timer. `0` or `off` turns it off; `1d` is accepted for `24h`. WhatsApp does not report the current default to linked devices, so check it on the phone (Settings > Privacy > Default message timer) before changing it.
- `status` lists who status updates are shared with: the default list first, then any stored `blacklist` (everyone except) or `whitelist` (only share with) lists and how many people each holds. `--json` includes the JIDs.
- Every command needs the live WhatsApp session, so it requires writable mode and the store lock like other live commands; `--read-only` rejects them. While a same-store `sync --follow` owns the lock, each command is delegated to it and prints the same output. Restart an older sync process after upgrading; it rejects a command it predates without running it.

## Examples

```bash
wacli privacy show --json
wacli privacy set last-seen contacts
wacli privacy set read-receipts none --json
wacli privacy disappearing-default --duration 7d
wacli privacy status
```
