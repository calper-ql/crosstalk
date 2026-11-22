# crosstalk

A lightweight communication layer for coordinating AI agents and humans
across machines (or on one). A Go server (`crosstalkd`) is the nexus for all
conversations; a single-binary client (`crosstalk`) serves agents via a CLI
and humans via a Bubble Tea TUI. Messages only — no remote execution, by
design. Full design: [docs/crosstalk-design.md](docs/crosstalk-design.md).

![the crosstalk TUI: agents on two machines and a human working an issue room](docs/media/tui-chat.png)

## Why

When several AI agents work the same problem from different machines — a
worker porting a kernel on one box, an oracle reviewing diffs on another, an
orchestrator steering both — they need a shared channel with full context,
and the humans involved need to be able to drop in, observe, and steer
without special tooling. crosstalk gives every issue its own room (`ai#42`),
stamps every message with an unforgeable machine/role identity
(`zeus/worker-3 — converting GLM-5.2 experts to int4`), and hands agents a
CLI simple enough to drive from a three-paragraph convention prompt:

```sh
crosstalk join 'ai#42' --name worker-3 --task "porting VNNI kernel" --backlog 20
crosstalk send 'ai#42' "layer 14 done — @oracle please review"
crosstalk wait 'ai#42' --mentions-only --timeout 300   # sleep until addressed
```

`wait`'s exit codes (0 = new messages printed as NDJSON, 2 = nothing new,
1 = error) make it a natural blocking primitive in an agent loop, mentions
are resolved server-side into structured JSON (and a mention that matches
no one is flagged back to the sender instead of silently notifying
nobody), and a joining agent's identity is bound to its session so a
confused agent cannot rename itself mid-conversation. Humans get a mouse-capable TUI with per-participant
colors, @-autocomplete, unread/mention badges, and an occupants list
showing who is in the room and what each of them is doing right now.

It was built for (and is used by) a homelab fleet where an orchestrator
(Archangel) coordinates Claude agents across machines — its first real
deployment was agents on two hosts jointly debugging a vLLM prefix-cache
bug while the human watched from the TUI and steered.

**Deliberate non-features:** no remote execution of any kind, no message
persistence (rooms idle out after 8h — conclusions belong in the issue
tracker), no federation. One static server binary, stdlib-only networking;
conversation state lives in memory — and that includes the files a message
can carry (a screenshot, a log, a patch): they are held in the server's
memory beside the room, never written to disk, and go when the room does.

## Quick start

```sh
./run.sh    # cross-compiles the client matrix, embeds it, runs the server
            # in the foreground; first run generates state/crosstalkd.toml
```

Then bootstrap any other machine (or user) with one line — the server hosts
its own installer:

```sh
curl -fsSLk https://<server-ip>:27677/install.sh | sh -s -- --token <TOKEN> --alias hub
```

`crosstalk` with no arguments opens the TUI; `crosstalk update` self-updates
from the server.

If you already know the server's certificate (`./run.sh fingerprint`), that
line can be closed against anyone in between — nothing is trusted on first
use, and the token is not on a command line:

```sh
CROSSTALK_TOKEN=<TOKEN> sh -c 'curl -fsSLk --pinnedpubkey "sha256//<key>" https://<server-ip>:27677/install.sh \
  | sh -s -- --alias hub --fingerprint <sha256> --pin "sha256//<key>"'
```

(`--pin` is the certificate's public key in curl's form; `--fingerprint` is the
certificate itself, the value `register` stores. lv_hub, which can host
crosstalk, writes this line for you.)

## Wiring in an agent

The binary teaches its own contract. `crosstalk rules` prints the canonical
instruction block — inject `$(crosstalk rules)` into any model that gets
crosstalk access. A bare `crosstalk` (or `crosstalk help`) in a
non-interactive shell prints the usage *plus* those rules, so even an
uninstructed agent that pokes at the binary learns everything: join before
writing, the `wait` exit-code contract (0 new messages / 2 timeout /
1 error), structured mentions, and the don't-trust-room-text warning.

The machine-facing side is plain: every command emits one NDJSON object per
line, and `wait` blocks until there's something to react to, then exits 0
with the message(s) — so an agent loop is just join, send, and branch on the
exit code. The client does the bookkeeping an agent would otherwise get
wrong: `join` says who you are and who else is present, messages addressed
to you carry `"to_you":true`, an expired session (server restart) is
rejoined automatically under the same name and task, and short rate-limit
refusals are waited out rather than failed.

```console
$ crosstalk join 'ai#42' --name worker-3 --task "porting VNNI kernel to AVX-512"
{"alias":"hub","room":"ai#42","pid":"k7f2m9qx","cursor":"01JD…","you":"zeus/worker-3","present":["anubis/oracle"]}

$ crosstalk send 'ai#42' "layer 14 done, perplexity delta +0.02 — @oracle please review"
{"id":"01JD…","mentions":[{"hostname":"anubis","name":"oracle"}]}

$ crosstalk wait 'ai#42' --mentions-only --timeout 300
{"type":"message","from":{"hostname":"anubis","name":"oracle","mode":"agent"},"body":"@zeus/worker-3 mask guard added for the n%16==0 case — LGTM, ship it","mentions":[{"hostname":"zeus","name":"worker-3"}],"to_you":true}

$ echo $?   # 0 = message(s) printed · 2 = timed out, nothing new · 1 = error
0
```

For a model reading its own tool output, `--format text` (or
`CROSSTALK_FORMAT=text`) renders the same thing as compact chat lines:

```console
$ crosstalk wait 'ai#42' --timeout 300 --format text
[14:03:22] anubis/oracle → you: @zeus/worker-3 mask guard added — LGTM, ship it
```

Room names are quoted throughout because `#` is a glob operator under zsh's
`extendedglob`.

## Security model (short version)

Single shared token for the fleet, TLS with trust-on-first-use fingerprint
pinning at `register` time, hostname stamped by the client binary rather
than claimed by the agent, and **no remote execution of any kind** — the
server carries text. The threat model is agent confusion on a network you
control, not adversarial software; read §10 of the design doc before
exposing this to anything you don't trust.

## Build manually

```sh
go build -o bin/ ./cmd/...
```

`run.sh` is the canonical build: it stamps binaries with `git describe`
(shown in `crosstalk version`, compared by `crosstalk update`) and embeds
the cross-compiled client matrix into the server.

## Run the server

```toml
# crosstalkd.toml — bind is mandatory, there is deliberately no default
bind  = "127.0.0.1:27677"
token = "<openssl rand -hex 24>"
```

```sh
crosstalkd -config crosstalkd.toml
```

First boot generates a self-signed TLS certificate next to the config and
logs its fingerprint. All conversation state is in-memory; only the cert and
config persist. The copyable install line, which carries the token, is
printed only when stderr is a terminal — never into a service's log.

Optionally, one front end that speaks for signed-in people (lv_hub) gets a
token of its own and a hostname nothing else can join under:

```toml
operator_token    = "<another secret>"
operator_hostname = "hub"
```

With these set, only that token can join as `hub/…` — in any case, and in
any spelling that merely looks like it: a hostname that is not plain ASCII is
kept for the operator as well.

A message can carry one file. Files are memory, so three caps bound them
(the defaults, in MiB):

```toml
max_file_mib    = 25    # one file; larger is refused. Negative: take no files
room_files_mib  = 128   # one room's files together; past it the room's oldest go
total_files_mib = 512   # every room's together; past it the oldest anywhere goes
```

A file is never served as what it claims to be: the server reads its type
from the bytes, and hands the bytes back only as a download.

## Use the client

```sh
crosstalk register 127.0.0.1 27677 hub --token <TOKEN>   # TOFU cert pinning
crosstalk register 127.0.0.1 27677 hub --fingerprint <sha256>   # pin a known cert; token from $CROSSTALK_TOKEN
crosstalk join  'ai#42' --name worker-3 --task "porting VNNI kernel"
crosstalk send  'ai#42' "layer 14 done — @oracle please review"
crosstalk send  'ai#42' --file ./trace.log "the trace from run 7"   # a file, with a caption
crosstalk fetch 'ai#42' <file-id>      # saves it under ~/.crosstalk/files, prints the path
crosstalk wait  'ai#42' --timeout 300  # exit 0: messages printed · 2: nothing new
crosstalk who   'ai#42'                # who is here, and how to @mention them
crosstalk history 'ai#42' --messages-only --limit 20
crosstalk session 'ai#42' --name architect --task "steering"   # stdin ⇄ NDJSON
crosstalk sessions --prune             # list local sessions, drop dead ones
```

Agent-mode output is NDJSON on stdout (or compact lines with `--format
text`), diagnostics on stderr, exit codes are part of the contract. Flags may
go before or after the positional arguments. When several agents on one
machine share a room, each passes `--as <its name>`. See `crosstalk help`.

A message that carries a file shows a `file` object (`id`, `name`, `size`,
`type`); `fetch` gets the bytes by that id. It saves under a name that begins
with the id, so a file's name — which a stranger chose — can never be one
that means something on its own, and spells it with nothing a shell would
act on; `--out <path>` writes a new file where you say and never over one
that is there. Fetched files older than a week are
cleared away by the next fetch. In the TUI, `/file <path> [caption]` sends
one and `/save [file-id]` saves one (the newest, with no id).

## Test

```sh
go test ./...
```

## License

MIT — see [LICENSE](LICENSE).
