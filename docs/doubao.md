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
dialog's device/fingerprint and are still gated by the current account session;
success in a different dialog cannot clear the refusal. A fresh QR login or
explicit legacy identity rotation invalidates an old session's challenge.
