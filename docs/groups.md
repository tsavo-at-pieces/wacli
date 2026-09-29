# groups

Read when: listing, refreshing, inspecting, renaming, joining, leaving, inviting, pruning stale local group rows, managing group participants or settings, setting a group photo, or managing WhatsApp Communities.

`wacli groups` combines local group rows with live WhatsApp operations. Commands that mutate WhatsApp require writable mode.

## Commands

```bash
wacli groups list [--query TEXT] [--limit N]
wacli groups refresh
wacli groups create --name NAME [--user PHONE_OR_JID ...] [--announce-only] [--locked] [--join-approval] [--community|--linked-parent GROUP_JID]
wacli groups info --jid GROUP_JID
wacli groups participants list --jid GROUP_JID
wacli groups rename --jid GROUP_JID --name NAME
wacli groups topic --jid GROUP_JID --text TEXT
wacli groups description --jid GROUP_JID --text TEXT
wacli groups announce-only --jid GROUP_JID (--on|--off)
wacli groups locked --jid GROUP_JID (--on|--off)
wacli groups join-approval --jid GROUP_JID (--on|--off)
wacli groups member-add-mode --jid GROUP_JID (--admins|--all)
wacli groups photo set --jid GROUP_JID --file PATH
wacli groups photo remove --jid GROUP_JID
wacli groups leave --jid GROUP_JID
wacli groups participants add --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups participants remove --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups participants promote --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups participants demote --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups requests list --jid GROUP_JID
wacli groups requests approve --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups requests reject --jid GROUP_JID --user PHONE_OR_JID [--user ...]
wacli groups invite link get --jid GROUP_JID
wacli groups invite link revoke --jid GROUP_JID
wacli groups invite info LINK_OR_CODE
wacli groups join --code INVITE_CODE
wacli groups prune [--days N] [--left-only=false|--include-active] [--dry-run] [--confirm]
wacli groups community subgroups --jid COMMUNITY_JID
wacli groups community participants --jid COMMUNITY_JID
wacli groups community link --parent COMMUNITY_JID --child GROUP_JID
wacli groups community unlink --parent COMMUNITY_JID --child GROUP_JID
```

## Notes

- Group JIDs use the `...@g.us` server.
- `list` reads local rows and hides groups marked left. Human output includes the group type (`group`, `community`, or `subgroup`) and parent community JID when known.
- `list --json` includes `IsParent` for communities and `LinkedParentJID` for subgroups.
- `list` returns at most 50 matching groups by default; non-positive `--limit` values also use 50. When more matching groups exist, stderr warns that the result is truncated (a warning event with `--events`); increase `--limit` to see more. JSON and table output keep their existing shape.
- `refresh` fetches joined groups live and updates local rows, including WhatsApp Community hierarchy metadata exposed by whatsmeow.
- `participants list` reads the last participant snapshot in `wacli.db` without connecting to WhatsApp. It works in read-only mode.
- A participant result can be empty or stale. Run `wacli sync --once --refresh-groups` or `wacli groups refresh` to fetch joined-group info and replace the stored snapshots. Normal sync also refreshes a group snapshot when it stores a message from that group and the live group-info lookup succeeds.
- Participant `updated_at` values record when wacli stored the snapshot. They are not WhatsApp join times or membership-change times.
- `info` fetches one group live and persists it, including whether the chat is a Community parent or linked subgroup.
- `create` returns the new live group info and persists it locally. Use `--community` to create a community parent, or `--linked-parent` to create a subgroup inside an existing community.
- `topic` and `description` both set the WhatsApp group description. Passing `--text ""` clears it. Each change first reads the group live and names the current description's ID, because WhatsApp rejects a description edit that does not name the one it replaces.
- `announce-only --on` makes the group admin-send-only; `--off` restores participant sends.
- `locked --on` makes group info editable only by admins; `--off` allows member edits again.
- `join-approval --on` makes admins approve people who join by invite link (see `requests`); `--off` lets them join directly.
- `member-add-mode --admins` lets only admins add participants; `--all` lets every member add them.
- `photo set` takes a JPEG or PNG, converts it to JPEG and scales it to at most 640px on its longer side, as `profile set-picture` does. When `WACLI_MEDIA_ROOTS` is set, the file must be inside one of those directories, as for `send file`. The conversion happens in the calling process, so a running `sync --follow` receives image bytes, never a path. `photo remove` clears the photo.
- Settings changes (`topic`, `description`, `announce-only`, `locked`, `join-approval`, `member-add-mode`, `photo`) and join-request approvals refresh the group's local snapshot afterwards. The local store keeps the group's name, owner, community hierarchy and participants, not its settings or photo; use `info --json` to see the live settings (`IsAnnounce`, `IsLocked`, `IsJoinApprovalRequired`, `MemberAddMode`, `Topic`).
- `requests` lists, approves, or rejects pending join requests for groups with join approval enabled.
- `invite info` previews the group behind an invite link (`https://chat.whatsapp.com/...`) or bare code without joining it. It stores nothing locally. Human output shows the group's reported size, since a preview lists few participants or none.
- `community subgroups` lists a community's linked groups live, marking the default (announcements) group. `community participants` lists members across the community's linked groups, with phone numbers where the session can resolve a LID.
- `community link` adds an existing group to a community you administer; `community unlink` removes it from the community without deleting the group. Both refresh the child group's local row so its parent community (`LinkedParentJID`) matches.
- `requests list`, `invite link get`, `invite info`, `community subgroups` and `community participants` only read from WhatsApp, but still need a connection. Read-only mode never connects, so they fail in read-only mode, including when a `sync --follow` is running.
- `leave` marks the group left locally after WhatsApp confirms.
- `prune` only deletes local group/chat/message rows from `wacli.db`. It does not leave WhatsApp groups or delete anything from WhatsApp servers.
- `prune --dry-run` only reads the store and does not take the store lock, so it works while `sync --follow` runs.
- `prune` defaults to groups marked left locally. `--days N` limits left-group pruning to groups left more than `N` days ago.
- `prune --include-active --days N` also targets active groups whose last known local message is older than `N` days. Groups with no known local activity timestamp are skipped.
- Use `prune --dry-run` before deleting and `--confirm` only after reviewing the target list.
- Participant users accept phone numbers with common formatting or JIDs.
- Invite `revoke` resets the invite link.
- `list` and `participants list` read the local store without the store lock, so they work while `sync --follow` runs.
- While a same-store `sync --follow` owns the store lock, every other `groups` command is delegated to it and prints the same output as a direct run: `create`, `info`, `rename`, `topic`, `description`, `announce-only`, `locked`, `join-approval`, `member-add-mode`, `photo set|remove`, `leave`, `join`, `refresh`, `invite link get|revoke`, `invite info`, `participants add|remove|promote|demote`, `requests list|approve|reject`, `prune`, and `community subgroups|participants|link|unlink`. Read-only checks and input validation (JIDs, flags, the photo file and `WACLI_MEDIA_ROOTS`) happen in the calling process before anything is delegated. Restart an older sync process after upgrading; it rejects a command it predates without running it.
- A delegated `prune` lists its targets and asks for confirmation (unless `--confirm`) in the calling process, then the sync process deletes only the confirmed groups that are still prunable under the same `--days`/`--include-active` criteria when it runs. Without a running sync process, `prune` keeps the store-lock error and does not prompt.

## Examples

```bash
wacli groups list --query family
wacli groups refresh
wacli groups create --name "Project launch" --user "+1 (234) 567-8900" --announce-only
wacli groups info --jid 123456789@g.us
wacli groups rename --jid 123456789@g.us --name "New name"
wacli groups topic --jid 123456789@g.us --text "Launch planning"
wacli groups announce-only --jid 123456789@g.us --on
wacli groups join-approval --jid 123456789@g.us --on
wacli groups member-add-mode --jid 123456789@g.us --admins
wacli groups photo set --jid 123456789@g.us --file ./team.png
wacli --read-only groups participants list --jid 123456789@g.us --json
wacli groups participants add --jid 123456789@g.us --user "+1 (234) 567-8900"
wacli groups requests approve --jid 123456789@g.us --user "+1 (234) 567-8900"
wacli groups invite link get --jid 123456789@g.us
wacli groups invite info https://chat.whatsapp.com/AbCdEfGhIjK
wacli groups join --code AbCdEfGhIjK
wacli groups prune --dry-run
wacli groups community subgroups --jid 120363000000000009@g.us
wacli groups community link --parent 120363000000000009@g.us --child 123456789@g.us
```
