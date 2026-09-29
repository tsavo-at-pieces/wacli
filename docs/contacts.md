# contacts

Read when: finding synced contacts, importing macOS Contacts names, managing local contact metadata, saving or deleting WhatsApp contacts, or blocking users.

`wacli contacts` works with contact metadata stored locally and with the account's WhatsApp contacts. Aliases, tags and imported system names are local to `wacli`; they do not edit WhatsApp contacts on the phone. `save`, `delete`, `block` and `unblock` change the account on WhatsApp itself.

## Commands

```bash
wacli contacts search <query> [--limit N]
wacli contacts show --jid JID
wacli contacts resolve <lid|phone|jid> [...]
wacli contacts check <phone> [phone...]
wacli contacts refresh
wacli contacts import-system [--input FILE] [--dry-run] [--clear]
wacli contacts alias set --jid JID --alias NAME
wacli contacts alias rm --jid JID
wacli contacts tags add --jid JID --tag TAG
wacli contacts tags rm --jid JID --tag TAG
wacli contacts save (--jid JID | --phone PHONE) [--first-name NAME] [--full-name NAME] [--save-to-phone]
wacli contacts delete --jid JID
wacli contacts block --jid JID
wacli contacts unblock --jid JID
wacli contacts blocklist
```

## Notes

- `search` matches alias, full name, push name, first name, business name, phone, and JID.
- `search` and `show` combine phone-number and `@lid` contact rows only when the local WhatsApp session has a verified mapping. Matching names alone never merge contacts. The combined result uses the phone-number JID and its actual phone number; an unmapped `@lid` stays separate with an empty `phone` field.
- Search matches metadata and stored IDs on either row, as well as the resolved phone number. `--limit` applies after combining duplicates. A contact stored only as `@lid` is also searchable by its mapped phone number, including partial numbers.
- For combined contacts, local aliases and system names retain their display precedence, with the phone-number row winning conflicts within each field. `show` accepts either stored JID or the resolved phone-number JID and combines tags. Reading contacts never rewrites their stored rows or metadata.
- Alias and tag commands accept either verified identity, including the JID returned by `search` or `show`. Changes apply atomically to both identities so removing metadata cannot reveal an older copy on the other row. These local commands read the session mapping without connecting to WhatsApp or modifying its session database; an unreadable session database fails the command before a metadata write.
- `resolve` maps each LID to its phone number and each phone number to its LID, using the local session's verified mapping. It reads local state only and takes no store lock, so it works while `sync --follow` is running. Every input gets an entry: an identity without a known pair has `resolved: false` (never dropped), and a group, channel or malformed input carries an `error`. JSON entries have `input`, `jid` (phone JID), `phone`, `lid`, `name`, `resolved` and `error` (#420).
- `check` connects with the account session and asks WhatsApp's servers whether each number is registered (accepts +E164, common formatting, or user JIDs). Results are reported per query and not stored locally; use `--json` for scripting. A number the server did not answer for is reported as `no response` (JSON `"responded": false`) — treat it as unknown, not as a confirmed negative.
- `refresh` imports contacts from the whatsmeow session store into `wacli.db`.
- `import-system` imports display names from macOS Contacts by matching phone numbers against already-synced wacli contacts. Run `contacts refresh` first.
- `import-system --input FILE` reads a JSON array or newline-delimited JSON contacts file with `full_name` and `phones` fields instead of opening macOS Contacts.
- Imported system names are local wacli metadata. They do not edit WhatsApp contacts or macOS Contacts.
- Display precedence is local alias, imported system name, then WhatsApp names.
- Use `import-system --dry-run` before writing. Use `import-system --clear` to remove imported system names.
- See [contacts import-system](contacts-import-system.md) for the full import workflow, JSON shape, file format, and verification steps.
- Tags are local grouping metadata for scripts and future workflows.
- While a same-store `sync --follow` owns the store lock, `alias set|rm`, `tags add|rm`, and `refresh` are delegated to it. It writes with its own open store and resolves identities from the same session mapping, still without contacting WhatsApp, and the command prints the same output as a direct run. Restart an older sync process after upgrading; it rejects a command it predates without running it.
- `check`, `import-system` (writes and `--clear`), `save`, `delete`, `block`, `unblock`, and `blocklist` are delegated the same way, so they work while `sync --follow` runs. `check` runs on the sync process's connected session and uses the same request kind (`contacts_check`) and fields as upstream wacli. `import-system` reads macOS Contacts or `--input` in the invoking process and hands the sync process only the phone-to-name map; `--dry-run` never takes the lock and runs directly. Delegated operations share the sync process's serialized queue, so a long `check` list can hold a queued send until that send's own `--timeout`; split long lists.

## WhatsApp contacts

- `save` writes an entry to the account's WhatsApp contacts, the address book WhatsApp syncs to the phone and every linked device, or renames an existing entry. Pass `--jid` (a phone number, phone-number JID, or a LID whose phone number the session knows) or `--phone`. `--full-name` defaults to `--first-name`; at least one is required. `--save-to-phone` also asks the phone to add the contact to its own address book.
- Entries are keyed by phone-number JID, as WhatsApp's own clients write them; the paired LID from the session's verified mapping (or a WhatsApp lookup when none is stored) travels in the entry. A LID with no known phone number cannot be saved.
- `delete` removes the entry for either identity of the person. It only sends a change when the session's app state holds an entry for that person; otherwise it reports `no saved WhatsApp contact` and changes nothing. Entries written under an app-state key older than the session's newest key are not found. Chats, messages, aliases, tags and push names are kept.
- After `save` or `delete`, the saved names in `wacli.db` are updated at once, so `show` and `search` reflect them (subject to the usual precedence: alias, then system name, then WhatsApp names). `sync` also mirrors contacts saved or renamed on the phone or other devices. whatsmeow does not report contact removals made on other devices, so a contact deleted elsewhere keeps its old saved name locally until `save` or `delete` runs here.
- JSON output: `save` returns `jid`, `lid`, `full_name`, `first_name`, `save_to_phone`; `delete` returns `jid`, `lid`, `deleted`, and `removed` (the entry identities that existed). If WhatsApp was updated but the local store write failed, the result carries `store_warning` and a warning goes to stderr.

## Block list

- `block` and `unblock` accept a phone number, phone-number JID or LID. whatsmeow resolves the LID WhatsApp needs for the change.
- `blocklist` fetches the block list from WhatsApp. JSON is `{"count": N, "blocked": [{"jid", "phone_jid"}]}`; WhatsApp usually lists LIDs, and `phone_jid` is filled from the session's mapping when known.
- wacli keeps a local copy of the block list: `block`/`unblock` update it for both identities, `blocklist` replaces it with the fetched list, and `sync` applies block and unblock notifications from other devices. `show` prints `Blocked: yes` (JSON `"blocked": true`) for a contact on that copy. A notification that only says the list changed carries no entries; run `blocklist` to refresh.

## Examples

```bash
wacli contacts search Alice
wacli contacts show --jid 1234567890@s.whatsapp.net
wacli contacts resolve 123456789@lid 987654321@lid --json
wacli contacts check +43 664 12345678 --json
wacli contacts refresh
wacli contacts import-system --dry-run
wacli contacts alias set --jid 1234567890@s.whatsapp.net --alias mom
wacli contacts tags add --jid 1234567890@s.whatsapp.net --tag family
wacli contacts save --phone "+1 202 555 0142" --first-name Alex --full-name "Alex Example"
wacli contacts delete --jid 12025550142@s.whatsapp.net
wacli contacts block --jid +12025550142
wacli contacts blocklist --json
```
