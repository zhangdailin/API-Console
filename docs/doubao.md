# Doubao session maintenance

Doubao Web authenticates with the Cookie obtained through QR login. It does not
use an OAuth refresh-token grant. Normal first-party requests and passport health
checks now save root-path `Set-Cookie` rotations (including deletions) in the
encrypted account store. This preserves rotations supplied by the upstream; it
does not manufacture a new login after the upstream has expired the session.

Request-local clients re-read the current session under a process-local,
per-account credential gate before sending a first-party request. The gate ends
after response headers and Cookie persistence, not after the answer stream.
Generation-checked Redis updates protect against concurrent QR login or device
rotation, including updates from another process. This is not a distributed
request lock: multiple gateway instances may still send concurrent requests.
Conflicting Cookie commits stop without automatically replaying a generation.

Health observations update only session verdicts and observed identity, never
administrator settings or usage counters. Only an explicit HTTP 401 proves
Cookie rejection. An empty/unknown passport envelope, CSRF error, plain 403, or
temporary login-service failure must not be relabeled as an expired Cookie.
Passport success clears an old credential rejection but not a chat captcha or
rate-limit restriction. A genuinely rejected session needs another QR login.

The legacy credential format is a site-wide Cookie map, not a browser CookieJar:
it cannot retain per-path/domain/expiry metadata. Narrow-path and unrelated-domain
response cookies are not broadened into that map. First-party automatic redirects
are disabled so a redirect cannot forward the credential to another origin.

Regression tests use synthetic local upstreams. They prove persistence, conflict
guards, and error classification, not live-upstream session lifetime or a new
refresh API.

## Automatic conversation identity

Every inference dialog now receives its own pseudorandom 19-digit `device_id`
and `web_id`; `tea_uuid` equals that dialog's `web_id`. The random persisted
QR-login tuple seeds a domain-separated derivation scoped to the authenticated
caller and explicit conversation ID. The same dialog therefore keeps its tuple
across turns, ordinary Cookie refreshes, request-local clients and process
restarts. Another dialog, caller or QR-login identity gets another tuple.
The account's stored tuple and authentication verdict are not overwritten.

Supply `conversation_id` (including its existing compatible spellings) or
`X-Conversation-Id` to identify a continuing dialog. Without an explicit ID,
each inference request is independent: it gets a fresh random tuple and an
uncached child conversation, never another caller's account-wide history.
The upstream conversation cache remains process-local; a restart may recreate
the upstream child even though the derived device tuple remains stable.
Legacy accounts without the persisted QR tuple use their session fingerprint
as compatibility seed; their identity cannot survive a changed session Cookie.

Account rows retain one status/quota refresh action. There is no manual device
identity control and rendering/reloading the page never rotates identity.
Neither an HTTP 403 nor a human-verification challenge triggers a new tuple or
an automatic generation retry. Captcha records retain the exact refused
dialog's device/fingerprint in encrypted, short-lived Redis records shared by
replicas. An acknowledgement names the displayed challenge version; a success
in a different dialog cannot clear the refusal. A fresh QR login or
explicit legacy identity rotation invalidates an old session's challenge.

## OpenAI-compatible endpoints

The current Doubao chat stream ends with `SSE_REPLY_END` and
`end_type: 3`. Message and answer notifications (`1` and `2`) are intermediate.
When that sequence is present, the gateway waits for the final marker and
does not report partial text as a successful completion. Older stream formats
without this sequence retain their existing completion checks.

Use `/doubao/v1` (or `/doubao`) with a managed gateway API key. Chat Completions
supports JSON and SSE, the `doubao-think` selector, and idle SSE heartbeat
comments. When the upstream marks a reasoning segment, it is kept separate
from answer text. Messages and Responses are not offered by this channel.

| Method and path | Request and result |
| --- | --- |
| `POST /files` | Multipart field `file`; accepts a document or image and returns an opaque `file_db_…` ID. |
| `GET /files/{file_id}`, `DELETE /files/{file_id}` | Read file metadata or delete the temporary upload. |
| `POST /images/upload` | Multipart field `image` or `file`; PNG, JPEG, GIF or WebP. The returned ID can be used as a reference image. |
| `POST /images/generations` | JSON `prompt`, optional `model: "doubao-image"`, `ratio`, `n`, and `response_format: "url"` or `"b64_json"`. |
| `POST /images/edits` | Same image output options. Supply `image` or `images` as a public URL, base64, data URL, or an object with `file_id`. Multipart accepts binary `image` or repeated `image[]` fields. The default model is `doubao-image-edit`; `doubao-image` is also accepted. |
| `POST /video/generations` | Experimental. JSON or multipart; `prompt`, optional `model: "doubao-video"`, `ratio`, and optional reference images (same shapes as image edits). Waits for the submitted task and returns `data[].video_url` when a completed video is present. |

For chat, attach a previous upload as
`{"type":"file","file":{"file_id":"file_db_…"}}` in the latest user turn.
You can also send inline/base64 documents or image blocks and public attachment
URLs. PDF, TXT, Markdown, and code files are forwarded as attachments rather
than converted by a local document parser. Uploads expire after one hour, are
restricted to the creating key, and are encrypted in deployments configured
with a credential encryption key. Each key may have eight active uploads,
with a 16 MiB per-file and per-request attachment limit and up to four
attachments per request. Gateway file deletion removes the staged copy;
it does not delete attachments already sent to Doubao.

In the account console, Doubao supports QR sign-in or importing a browser Cookie
that the administrator already owns. Imports verify the session and reject
duplicate users before saving; refresh an existing account by QR sign-in.
The console can open the official site in the operator's browser and keeps
the existing interactive verification flow for provider challenges.

An explicit conversation ID binds to the selected account for 24 hours after
the latest turn; a request to that ID does not silently switch accounts.
Anonymous chat can retry once under the existing account policy before any
output. The upstream conversation itself is still cached in-process. A clear
daily video quota rejection affects only video, until the next Shanghai
midnight. A video request can try one other account on that rejection, but
an accepted task is polled on the original account and never re-submitted.

These standalone media adapters were tested against synthetic upstream responses.
They are not published as verified entries in the live model catalog.
A missing completed asset returns 502, and video generation remains
unverified against the live upstream. There is no guessed media pricing;
keys with a USD billing cap cannot issue these unpriced media generations.

The independent media contract and acceptance cases were informed by
[doubao2api](https://github.com/SeiShonagon520/doubao2api/tree/a33787d5421ec8bf4b9c6cab0be24e95e4433486).
