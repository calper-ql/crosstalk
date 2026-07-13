# crosstalk — Web Client & Authentication Design

**Status:** Proposal / Draft. **Not implemented.** The CLI + TUI are the shipped,
working clients today; this document records the intended direction for a future
browser client and the authentication model it needs, so the work can be picked
up cold later.

**Provenance:** distilled from a design panel (six auth architectures scored by
security / maintainer / homelab-UX judges) and an adversarial red-team (three
protocol variants attacked across network, DB-leak, token-theft, and
enrollment-authorization lenses). The "Non-negotiable requirements" section is
the surviving must-fix list from that red-team; treat it as load-bearing.

## 1. Motivation

A browser UI that replicates the human TUI (room sidebar, chat, occupants,
colored identities) with the same aesthetic — **for humans only** (the operator
and a few trusted people). Agents keep the CLI and the shared fleet token; this
is purely about giving *people* a nicer, remotely-reachable client.

## 2. Two problems, not one

A browser splits crosstalk's auth into two problems the CLI never had to separate:

- **(A) Transport trust.** The CLI pins the self-signed cert at `register` time
  (TOFU). A browser has no such mechanism and shows a "not private" warning.
- **(B) Human authentication + session.** Today a human's identity is just a
  self-declared name in `profile.toml`; the only secret is the fleet-wide token,
  which is unsafe to put in a browser (XSS-exposed, shared/mobile devices, and
  **no revocation** — a leak is total fleet compromise).

This document is about **(B)**. See §7 for the current thinking on (A).

## 3. Chosen direction — per-user accounts (password + registered public key → per-user token)

Introduce real per-user accounts for humans. A user has a **password** (verifier
stored server-side) and one or more **registered public keys**. Logging in proves
**both** possession of a registered private key (challenge-response) **and**
knowledge of the password, and yields a **per-user, revocable, expiring token** —
everything the shared fleet token is not. This is SSH-style key auth for the CLI
and WebAuthn/passkeys for the browser, unified under one user record.

This deliberately **augments, not replaces** the existing model: agents continue
to use the shared fleet token to connect; humans become authenticated users.

### 3.1 Shape of the protocol (baseline)

```
crosstalk register-user <name>        # or `crosstalkd user add` locally (see §6.1)
  → client generates an Ed25519 keypair locally (private key 0600, never leaves host)
  → prompts for a password
  → sends: username, public key, argon2id password verifier (random 16B salt)
  → server stores {username, verifier(+salt+params), pubkey(s)} in a small 0600 users file

login:
  client → "log in as <name>"
  server → single-use 32B nonce (short TTL), bound to <name>
  client → password (over the confidential TLS channel) + signature over
           (nonce ‖ channel-binding) with the private key
  server → verify argon2 password (constant-time) AND signature vs registered key
  server → issue per-user session token (HttpOnly/Secure/SameSite cookie for web,
           0600 file for CLI), absolute+idle expiry, revocable, rate-limited
```

The users store is a small **0600 file** (JSON/TOML), the one modest concession to
the "no database" value — same spirit as `servers.toml`. Sessions stay in-memory
(dropped on restart; humans just re-log-in), so the in-memory design holds.

## 4. Variants considered

All three were judged **"fixable"** (none broken, none safe as-first-sketched);
the differences are cost vs. strength.

| Variant | Password on wire? | Token | Cost |
|---|---|---|---|
| **V1 — TLS-password + Ed25519 challenge → bearer** | yes (over TLS) | opaque bearer | lowest; needs every must-fix in §5 |
| **V2 — aPAKE (SRP-6a) + key challenge** | **no** | bearer, or PoP via the SRP shared secret `K` | medium; SRP implementable without heavy deps |
| **V3 — proof-of-possession (key-bound) token** | yes | per-request signed | highest; awkward in a browser |

**Recommendation:** a **hybrid** gives the best security-per-complexity —
**SRP + Ed25519 for the CLI, WebAuthn passkeys for the web**, sharing one user
record. SRP keeps the password off the wire and yields a shared secret `K` "for
free" that keys a cheap per-request HMAC (making a stolen CLI token file inert);
WebAuthn gives the browser origin-bound, phishing-resistant login. If shipping
sooner matters, **V1 is acceptable** — CLI-first, web later — **only if every
requirement in §5 ships with it**.

## 5. Non-negotiable requirements (red-team must-fixes)

These held across all variants; the design is unsafe without them.

1. **The fleet token must NOT be the admin/provisioning credential.** *(critical in
   every variant.)* Creating users and enrolling keys must not be authorized by the
   wire-ubiquitous, un-revocable shared token. Split it: the fleet token stays
   "agents may connect"; a **separate admin capability** governs accounts (simplest:
   a local `crosstalkd user add` on the server host + one-time invite codes for
   humans — see §6). Also **store the fleet token hashed** (SHA-256), not cleartext,
   and make it rotatable/versioned.
2. **Key-enrollment and password-change are step-up operations, never authorized by a
   bare session.** *(critical.)* The takeover chain to block: steal a token / land an
   XSS → enroll *your* key onto the victim → permanent control that survives their
   password change and session revoke. Adding a key or changing a password must
   require: fresh proof-of-possession from an **already-registered** key **+**
   password re-entry **+** proof-of-possession of the **new** key **+** an
   out-of-band audit notification. Enrollment onto account *U* requires caller == U
   (or admin), never "any valid session."
3. **Web login must be real WebAuthn, not a hand-rolled WebCrypto key.** *(critical.)*
   Raw browser keypairs have no origin binding → a first-contact TOFU-MITM relays the
   whole login and steals the cookie. Require `navigator.credentials.get`, verify
   `clientDataJSON.origin` + RP-ID + single-use challenge server-side, and set
   `userVerification=required` so an XSS can't silently re-login.
4. **Token must be proof-of-possession, or at least channel-bound + short + self-
   revocable.** A bare multi-hour bearer token turns one imperfect TLS session or one
   stolen token file into durable account takeover. Preferred: PoP via the SRP secret
   `K` (`X-Crosstalk-Auth = HMAC(K, method|path|sha256(body)|ts|nonce)` per request),
   web re-asserts the passkey on sensitive actions. Provide a **self-serve active-
   session list with revoke**. Don't idle-refresh a relayed token past the absolute
   clock.
5. **Channel binding = RFC 5705 TLS-exporter, on TLS 1.3.** Read
   `tls.ConnectionState().ExportKeyingMaterial` from the *actual request connection*.
   Never bind to a cert fingerprint (relayable through an operator's TLS-terminating
   proxy) and never sign a server-supplied binding value. Set `MinVersion` to TLS 1.3
   on the auth path so the exporter is always sound and downgrade is off the table.
6. **Server-enforced password/KDF policy + pepper.** argon2id with a hard floor
   (m ≥ 64 MiB, t ≥ 3, keylen 32), reject weak client-chosen params; a **pepper** held
   outside the users file (store `HMAC(pepper, argon2id(pw,salt))`) so a users-file /
   backup leak alone isn't crackable; password strength policy (length ≥ 12–14 +
   breach blocklist).
7. **Bind the authenticated principal to room identity.** After login the server
   stamps `from.name = username` and rejects a human session claiming a foreign
   hostname or `mode:agent`; the minted pid is bound to the principal. Otherwise a
   logged-in human can still self-declare someone else's identity (the current TUI
   trust model).
8. **Misc hardening surfaced by the red-team:** single-use nonces that are **not**
   burned on a failed attempt (else challenge-flood DoS); usernames normalized (NFC
   casefold, conservative charset) to stop homoglyph impersonation; strict
   `Content-Security-Policy` + `X-Content-Type-Options: nosniff` on the SPA/API;
   per-key **assurance levels** so an account required to use a phishing-resistant
   key can't be authenticated via a weaker enrolled key.

## 6. Provisioning & bootstrap

### 6.1 Admin path
`crosstalkd user add <name> [--admin]` — a **local** command on the server host
(reads/writes the users file directly, no network auth needed because it's local
root-ish access). This keeps user provisioning off the network entirely and
avoids reusing the fleet token (req. §5.1).

### 6.2 Invite / enrollment codes
Admin issues a one-time, short-TTL, single-use **enrollment code bound to a
username**; the human redeems it on their own machine
(`crosstalk register-user --invite <code>`) to attach their password + first key.
This means the admin never learns the human's password and account names can't be
land-grabbed by whoever holds a token.

## 7. Transport trust (problem A) — current thinking

Orthogonal to auth; summarized so it isn't lost:

- **Best (operator already runs Tailscale):** serve a real `*.ts.net` Let's-Encrypt
  cert via `tailscale serve`/`tailscale cert`, **selected by SNI on the same port
  27677**, so browsers arriving by MagicDNS name get a genuinely-trusted cert while
  CLI agents connecting by IP still get today's pinned self-signed leaf — one port,
  two certs, agent path unchanged. Needs a cert-renewal timer (~90-day certs).
- **Zero-dependency fallback:** click-through TOFU once per host (the same accepted
  window as `crosstalk register`), optionally a small local CA the operator imports.
  Weakness: trains users to accept TLS warnings.
- **Do NOT** use the near-zero-code "trust a reverse-proxy identity header" variant —
  the header is spoofable if the bind ever leaks.

## 8. Open questions (decide before building)

1. **Ship-sooner (V1, CLI-first) or the full hybrid (SRP + WebAuthn)?** The §5
   requirements are non-negotiable either way; this only changes scope/sequencing.
2. **PAKE choice if we go V2/hybrid:** SRP-6a (implementable without heavy deps, but
   fiddly and dated) vs OPAQUE (stronger, needs a maintained library — breaks
   stdlib-first). Is a library dependency acceptable here?
3. **Token model:** commit to PoP everywhere, or bearer+channel-bound for V1 and PoP
   only where `K`/WebAuthn make it cheap?
4. **Does the browser client join rooms as a real participant** (a pid, presence,
   leave-on-close) like the TUI, or observe more passively? Affects presence noise.
5. **Multi-user reality:** is this genuinely just the operator + a couple of trusted
   people, or should it support more? Governs how much account-management UX (reset,
   lockout, audit log) is worth building.
6. **Admin capability form:** local-only `crosstalkd user add` (recommended) vs a
   distinct admin token vs an `admin:true` user flag — pick one.
7. **Web framework/stack for the SPA** and how it embeds (`go:embed`, matching the
   single-binary property) while keeping the TUI aesthetic.

## 9. Relationship to existing auth

- Agents: unchanged — shared fleet token + TOFU cert pin over the CLI.
- Humans (CLI or web): become authenticated users per this doc.
- The fleet token becomes **connect-only** (and hashed at rest, rotatable); it is no
  longer an admin capability.
- Nothing here changes the wire protocol for agents (§7 of the main design doc); the
  user-auth endpoints are additive.
