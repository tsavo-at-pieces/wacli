# media

Read when: downloading media from a synced message, or transcribing voice notes.

`wacli media` downloads media referenced by messages already stored in `wacli.db`, and transcribes stored audio with a local speech-to-text engine.

## Commands

```bash
wacli media download --chat JID --id MSG_ID [--output PATH]
wacli media backfill [--chat JID] [--limit N] [--workers N]
wacli media retry [--chat JID] [--type TYPES] [--before YYYY-MM-DD] [--limit N] [--batch N] [--wait DUR]
wacli media retry --chat JID --id MSG_ID [--batch N] [--wait DUR]
wacli media transcribe --chat JID --id MSG_ID [--force] [--engine NAME]
wacli media transcribe --pending [--chat JID] [--after DATE] [--before DATE] [--limit N] [--engine NAME]
```

## download

Downloads media for a single message.

### Notes

- The target message must already be synced.
- Media downloads are capped at 100 MiB.
- `--output` may be a file path or directory.
- If `--output` is omitted, media is written under the store media directory.
- `--read-only` is supported only with explicit `--output`; it writes the file without opening the WhatsApp session store or recording `local_path` / `downloaded_at`.
- Because it never opens the store for writing, `--read-only` also takes **no store lock**.
- While a same-store `sync --follow` is running (it holds the store lock for its whole run), a plain `media download` is handed to it: the sync process downloads with its own connection into the store media directory, or into `--output` (resolved against your current directory), records `local_path` and `downloaded_at` exactly as a direct run does, and the command prints the same output. There is no need to stop sync. See [While sync runs](#while-sync-runs).
- If the lock is held by something that does not accept delegated commands (a `sync --once`, another write command, or a follow process still starting up), the command fails and names `--read-only --output PATH` as the way to fetch the file without the lock. `--lock-wait` only turns the immediate failure into a timeout.

### Examples

```bash
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123 --output ./downloads
wacli media download --chat 1234567890@s.whatsapp.net --id ABC123 --output ./photo.jpg
wacli --read-only media download --chat 1234567890@s.whatsapp.net --id ABC123 --output /tmp/photo.jpg
```

## backfill

Downloads media for every already-synced message that has downloadable metadata
but no local copy yet, over a single connection.

`sync --download-media` only downloads media for messages that *arrive during*
the sync session. Media for messages synced earlier is never fetched by sync;
`media backfill` closes that gap by scanning existing rows.

### Notes

- Files are written under the store media directory (same layout as `download`).
- `--chat` scopes the backfill to a single chat JID.
- `--limit` caps how many files to download (0 = all); newest messages first.
- `--workers` sets the number of concurrent downloads (default 4).
- Runs until completion or interruption by default; explicitly set global `--timeout` to cap a run.
- Requires a writable store; not available in `--read-only` mode.
- Reports counts: pending (total matching), attempted, downloaded, skipped, failed.
- Runs inside a same-store `sync --follow` when one is running; see [While sync runs](#while-sync-runs).

### Examples

```bash
wacli media backfill                                   # download all pending media
wacli media backfill --limit 50                        # download the 50 newest pending
wacli media backfill --chat 1234567890@s.whatsapp.net  # one chat only
wacli media backfill --json                            # machine-readable counts
```

## retry

Recovers media that expired off WhatsApp's CDN. `media download` and `media
backfill` fetch directly from WhatsApp's servers, which only keep media for a
limited time — older media returns HTTP 403. `media retry` instead asks the
primary device (your phone) to re-upload the media via WhatsApp's media-retry
protocol (the same mechanism WhatsApp Web uses), then downloads it.

Recovery only works while the phone is online and still holds the media. Media
the phone no longer has is marked unavailable only after the stored CDN path is
also confirmed expired, so later runs can skip genuinely unavailable rows.

### Notes

- Requires a writable store and an online phone; not available in `--read-only` mode.
- Run `media backfill` first; retry is intended for media whose direct CDN download failed.
- Retry receipts are sent in batches (`--batch`, default 32) with a second
  attempt for non-responders; `--wait` (default 30s) bounds each attempt.
- `--chat` scopes to one chat; `--before YYYY-MM-DD` scopes to media older than a date.
- `--type` scopes to media types, comma-separated: `image`, `video`, `gif`,
  `audio`, `document`, `sticker`. `video` includes GIFs (WhatsApp sends them as
  looping video); `gif` selects only them.
- `--limit` caps how many messages to retry (0 = all pending); newest first,
  across all types unless `--type` is given.
- `--chat JID --id MSG_ID` retries exactly one message. It is not limited to
  the pending set: it also retries media already marked unavailable or recorded
  as downloaded (for example after its file was deleted). A message that does
  not exist in that chat, was deleted, or has no download metadata is an error.
  `--id` cannot be combined with `--type`, `--before` or `--limit`.
- Without `--id` and `--type`, the selection is unchanged: the newest pending
  media of any type.
- Runs until completion or interruption by default; explicitly set global `--timeout` to cap a run.
- Reports counts: requested, recovered, not_on_phone (gone), no_response, failed.
- `no_response` means the phone did not answer in time (often transient) — those
  stay pending and can be retried later; only `not_on_phone` is marked gone.
- Runs inside a same-store `sync --follow` when one is running; see [While sync runs](#while-sync-runs).

### Examples

```bash
wacli media retry                                    # try to recover all pending media
wacli media retry --chat 1234567890@s.whatsapp.net   # one chat only
wacli media retry --before 2026-01-01                # only media older than a date
wacli media retry --limit 50 --wait 45s --json       # bounded run, machine-readable
wacli media retry --type audio --limit 20            # the 20 newest voice notes and audio
wacli media retry --chat 1234567890@s.whatsapp.net --id ABC123   # exactly one message
```

## While sync runs

`sync --follow` holds the store lock and the WhatsApp connection for its whole
run. `media download` (without `--read-only`), `media backfill` and
`media retry` do not need it stopped: when the store is locked by a same-store
follow process, the command hands the work to it over the local delegate
socket, and the follow process runs it with its existing connection and keys.

- The output is the same as a direct run, with or without `--json`. Progress
  events and warnings (`media_backfill_start`, `media_download_failed`,
  `media_retry_progress`, ...) are streamed back while the work runs and
  printed on stderr as NDJSON with `--events`, or as the same stderr lines
  without it.
- `--read-only` and flag validation are checked before anything is handed over.
- These are long jobs, so they do not wait in the queue that serializes sends:
  sends and other delegated commands keep flowing while a backfill runs. The
  follow process runs at most 4 jobs at a time, and at most one bulk
  `media backfill` and one bulk `media retry` (a single-message `--id` retry or
  a download can run beside them). A job still waiting for its turn when its
  timeout passes is refused and never runs.
- A job's budget is the global `--timeout` when you set it explicitly (and
  always for `media download`, whose default is 5m); otherwise it runs until
  done, as a direct run would. Interrupting the command (Ctrl-C) or killing it
  closes the connection, and the follow process stops the job. Work finished
  before that (downloaded files, recorded paths) is kept.
- A follow process started before this support rejects the command without
  running it and reports that the running sync process does not support it;
  restart `wacli sync` after upgrading.

## transcribe

Turns stored voice notes (and any other `audio` message) into text on this
machine. Nothing is sent to a cloud service: ffmpeg converts the audio to
16 kHz mono PCM16 WAV and a local engine prints the transcript.

- `--chat JID --id MSG_ID` transcribes one message. A stored transcript is
  returned immediately; `--force` runs the engine again and replaces it.
- `--pending` transcribes every stored audio message without a transcript,
  newest first, and keeps going when one fails. This is the mode for a cron
  job or agent. `--chat`, `--after`, `--before` (RFC3339 or `YYYY-MM-DD`) and
  `--limit N` (0 = all) scope it.
- Targets that are not audio (text, images, documents) are rejected.
- Transcripts appear in `messages list/show/context/search/export`; see
  [messages](messages.md#voice-note-transcripts).

### Runs next to `sync --follow`, and in read-only mode

`media transcribe` never writes `wacli.db` or the WhatsApp session and takes
**no store lock**, so it works while `sync --follow` holds the lock:

- Message rows are read from `wacli.db` in SQLite read-only mode.
- The audio comes from the message's downloaded file when it still exists.
  Otherwise it is fetched straight from WhatsApp's CDN with the same direct
  download `wacli --read-only media download --output` uses (no session store,
  no lock). Downloads and the converted WAV go to a private (`0700`) temp
  directory that is always removed; `local_path` is not recorded.
- The only file it writes is `transcripts.db`.

### Expired audio

Older media is gone from the CDN (HTTP 403, 404 or 410). When that happens and
a same-store `sync --follow` is running, `media transcribe` asks it to run a
single-message `media retry` for that message (up to two 15 s attempts; about
90 s in all). The phone re-uploads the audio, the follow process downloads it
into the store media directory and records `local_path`, and the transcript is
made from that file (`source: "retry"`). Without a running follow process the
command reports the expired download with the command to run:
`wacli media retry --chat JID --id MSG_ID`. In `--read-only` mode it never asks
the phone (a retry writes the store), and says so. In `--pending` runs each
expired message is tried the same way; failures are recorded as usual.

Because transcription changes neither WhatsApp nor `wacli.db`, it is allowed
with `--read-only` / `WACLI_READONLY=1`. The one exception is the recovery of
expired audio through a running `sync --follow` described above, which only
happens without `--read-only`.

### Engines

Choose with `--engine NAME`, else `WACLI_TRANSCRIBE_ENGINE`, else `nemo-speech`.
Each engine runs as a subprocess without a shell; the WAV path is passed as a
single argument.

| Engine | Program (override var) | Command | Model (`WACLI_TRANSCRIBE_MODEL`) |
| --- | --- | --- | --- |
| `fluidaudio` | `fluidaudiocli` (`WACLI_FLUIDAUDIO`) | `fluidaudiocli transcribe {wav} --model-version MODEL` | default `ultra`: NVIDIA Parakeet "Ultra" on the Apple Neural Engine via CoreML. Fastest on Apple silicon. |
| `nemo-speech` (default) | `nemo-speech` (`WACLI_NEMO_SPEECH`) | `nemo-speech --quiet transcribe {wav} --model MODEL` | default `parakeet-tdt` (Parakeet v3); `nemotron-3.5` covers 40 locales. |
| `parakeet-cpp` | `parakeet-cli` (`WACLI_PARAKEET_CLI`) | `parakeet-cli transcribe --model MODEL --input {wav}` | required: path to a parakeet.cpp model file. |
| `whisper-cpp` | `whisper-cli` (`WACLI_WHISPER_CLI`) | `whisper-cli -m MODEL -f {wav} -l LANG -nt -np` | required: path to a ggml model file. `WACLI_TRANSCRIBE_LANGUAGE` sets `LANG` (default `auto`). |
| `command` | first word of `WACLI_TRANSCRIBE_COMMAND` | your template | optional; recorded with the transcript. |

Programs are found through the override variable, then `PATH`, then
`~/.local/bin`. A missing program or model fails before any audio is touched,
with an error naming the variable to set. Cron runs with a minimal `PATH`, so
set the override variables (or `PATH`) in the crontab.

`WACLI_TRANSCRIBE_COMMAND` is split into arguments by a small quote-aware
splitter: whitespace separates arguments, single or double quotes group text
containing spaces, and nothing is expanded (no `$VARS`, `~`, globs, pipes, or
backslash escapes). It must contain `{wav}`, which may sit inside a larger
argument such as `--input={wav}`:

```bash
export WACLI_TRANSCRIBE_ENGINE=command
export WACLI_TRANSCRIBE_COMMAND="'/opt/my stt/bin/stt' --lang en --input={wav}"
```

The engine's stdout is the transcript. Each line is trimmed and blank lines
are dropped. An empty transcript (silence) is stored and reported as empty; it
is not an error. stderr is captured, and its tail is included in error
messages.

### Environment

| Variable | Purpose |
| --- | --- |
| `WACLI_TRANSCRIBE_ENGINE` | Engine when `--engine` is not given (default `nemo-speech`). |
| `WACLI_TRANSCRIBE_MODEL` | Model name or path for the selected engine (see the table). |
| `WACLI_TRANSCRIBE_LANGUAGE` | whisper.cpp language (default `auto`). |
| `WACLI_TRANSCRIBE_COMMAND` | Command template for the `command` engine. |
| `WACLI_TRANSCRIBE_TIMEOUT` | Engine timeout per message as a Go duration (default `5m`). On expiry the engine is killed (on Unix, with its whole process group). |
| `WACLI_FFMPEG` | ffmpeg binary (default: `PATH`, then `~/.local/bin`). Each conversion has a 60 s timeout. |
| `WACLI_FLUIDAUDIO`, `WACLI_NEMO_SPEECH`, `WACLI_PARAKEET_CLI`, `WACLI_WHISPER_CLI` | Engine binary paths. |

The global `--timeout` applies only when you set it explicitly, as with
`media backfill`. Direct downloads are capped at 2 minutes each.

### Installing an engine

- **ffmpeg**: `brew install ffmpeg` or `apt install ffmpeg`.
- **FluidAudio** (macOS, Apple silicon): build the FluidAudio CLI and put
  `fluidaudiocli` on `PATH` or in `~/.local/bin`. The first run downloads and
  compiles the CoreML model (about 30 s); later runs are fast.
- **NeMo-Speech.cpp**: install `nemo-speech`, then fetch a model with
  `nemo-speech pull parakeet-tdt` (or `nemotron-3.5` for more languages).
- **parakeet.cpp**: build `parakeet-cli`, download a model, and set
  `WACLI_TRANSCRIBE_MODEL=/path/to/model`.
- **whisper.cpp**: `brew install whisper-cpp` (or build `whisper-cli`),
  download a ggml model such as `ggml-base.en.bin`, and set
  `WACLI_TRANSCRIBE_MODEL=/path/to/ggml-base.en.bin`.

### `transcripts.db`

Transcripts live in `<store>/transcripts.db`, a separate SQLite database
(WAL mode, `0600`) created on the first stored transcript. Read commands never
create it; without it, every command behaves as if no transcripts exist.

```sql
transcripts(chat_jid, msg_id, text, engine, model, duration_ms, created_at,
            PRIMARY KEY (chat_jid, msg_id))
transcript_failures(chat_jid, msg_id, error, attempts, last_attempt_at, ...)
```

- `chat_jid` uses the store's canonical chat identity: a `@lid` chat is
  resolved to its phone-number JID through the session's LID map, and device
  suffixes are dropped. Lookups accept either form, so a message still stored
  under its LID finds its transcript.
- `engine` and `model` record what produced the text; `duration_ms` is the
  audio length; `created_at` is Unix seconds.
- Transcripts are never written into `text`, `display_text`, or any other
  `wacli.db` column.
- Deleting `transcripts.db` removes every transcript and nothing else.

### Pending runs

`--pending` reports:

- `pending`: audio messages without a transcript that match the filters.
- `skipped`: pending messages with neither a downloaded file nor downloadable
  media metadata (or media the phone reported gone). They are never attempted.
- `attempted`, `transcribed` (including `empty`), `failed`.

A failure (for example, media that expired off the CDN) is recorded in
`transcript_failures`. Later runs try never-attempted messages first and
retry earlier failures after them, least recently tried first, so a `--limit`
window cannot get stuck on messages that keep failing. The command exits 0
when the run completes, even if items failed; the counts and per-item
`results` carry the detail. Setup errors (no store, unknown engine, missing
program or model) exit non-zero before any work starts. When nothing is
pending, no engine is needed and `transcripts.db` is not created.

### Output

Single message, `--json`:

```json
{"chat":"15550000001@s.whatsapp.net","id":"ABC123","transcript":"see you at the picnic","empty":false,
 "engine":"fluidaudio","model":"ultra","duration_ms":3800,"created_at":"2026-01-02T03:04:05Z",
 "cached":false,"source":"local","elapsed_ms":520}
```

`source` is `local` (downloaded file), `download` (fetched from the CDN for
this run) or `retry` (expired on the CDN and recovered from the phone through a
running `sync --follow`). The human form prints just the transcript.

`--pending --json` returns the counts plus `engine`, `model`, and `results`
(`chat`, `id`, `status`: `transcribed|empty|skipped|failed`, `detail`). Pending
output never includes transcript text, so it is safe to log.

### Examples

```bash
wacli media transcribe --chat 15550000001@s.whatsapp.net --id ABC123
wacli media transcribe --chat 15550000001@s.whatsapp.net --id ABC123 --force --engine whisper-cpp
wacli --read-only media transcribe --pending --limit 20 --json
WACLI_TRANSCRIBE_ENGINE=fluidaudio wacli media transcribe --pending --after 2026-01-01
```

A crontab that transcribes new voice notes every 10 minutes while
`sync --follow` runs (cron does not expand `$HOME`, so use full paths):

```bash
WACLI_TRANSCRIBE_ENGINE=fluidaudio
PATH=/Users/you/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin
*/10 * * * * wacli --read-only media transcribe --pending --json >> /Users/you/.wacli-transcribe.log 2>&1
```
