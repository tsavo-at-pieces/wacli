# profile

Read when: managing the authenticated WhatsApp profile or fetching profile metadata for a WhatsApp user.

`wacli profile` manages account-level WhatsApp profile settings for the linked account.

## Command

```bash
wacli profile set-picture <image>
wacli profile remove-picture
wacli profile set-about <text>
wacli profile set-name <name>
wacli profile picture-info --jid <jid-or-phone> [--preview] [--existing-id ID]
wacli profile get-about --jid <jid-or-phone>
wacli profile business --jid <jid-or-phone>
```

## Notes

- Mutating commands (`set-picture`, `remove-picture`, `set-about`, `set-name`) require authentication, a live connection, writable mode, and the store lock (or a running `sync --follow`, see below).
- Input can be JPEG or PNG.
- PNG transparency is flattened onto a white background before upload.
- Images larger than 640 px on either side are resized before upload.
- `set-picture` prints the picture ID returned by WhatsApp; use `--json` for machine-readable output.
- `set-about` updates the profile About text, not ephemeral status broadcasts. Use `send status` for WhatsApp status posts.
- `set-name` updates the WhatsApp push/display name through whatsmeow app-state support.
- `picture-info` fetches metadata and a CDN URL for the target's profile picture. Pass `--preview` for preview metadata or `--existing-id` to let WhatsApp report unchanged pictures.
- `get-about` fetches a target user's current About text.
- `business` fetches a target WhatsApp Business profile when available.
- Live fetch commands do not mutate WhatsApp profile settings, but they still open the WhatsApp session store and therefore cannot run with `--read-only`.
- While a same-store `sync --follow` owns the store lock, every `profile` command is delegated to it and prints the same output, with or without `--json`. `set-picture` reads and resizes the image in the invoking process and hands the sync process only the JPEG bytes. `--read-only` is still checked first. Restart an older sync process after upgrading; it rejects a command it predates without running it.
- Privacy settings (last seen, profile photo and About visibility, read receipts, and more) are under [privacy](privacy.md).

## Examples

```bash
wacli profile set-picture ./avatar.jpg
wacli profile set-picture ./avatar.png --json
wacli profile remove-picture
wacli profile set-about "Available today"
wacli profile set-name "Ops Desk"
wacli profile picture-info --jid +12025550142 --json
wacli profile get-about --jid 12025550142@s.whatsapp.net
wacli profile business --jid +12025550142 --json
```
