# Design notes

Why infisical-mirror is built the way it is. The code comments state the rule;
this file holds the reasoning that is too long to sit above a function.

## Contents

- [Shape of a run](#shape-of-a-run)
- [The state file](#the-state-file)
- [Guards](#guards)
- [Config validation](#config-validation)
- [Path handling](#path-handling)
- [Normalisation](#normalisation)
- [Talking to the API](#talking-to-the-api)
- [The webhook trigger](#the-webhook-trigger)
- [Keeping secrets out of output](#keeping-secrets-out-of-output)

## Shape of a run

A run is three phases with a hard boundary between each:

1. **Read.** Both sides are listed into a `Snapshot`, keyed by the folder path
   relative to that side's root. Relative keying is what lets two differently
   rooted trees be compared at all.
2. **Decide.** `reconcile.Reconcile` takes the two snapshots plus the last-synced
   state and returns a plan. It is pure: it reads no network and writes no state.
   That is what makes every row of the decision table testable without a server.
3. **Write.** `reconcile.Apply` carries out the plan and returns the state that
   actually landed.

The read side and the write side are separate interfaces (`Reader`, `Writer`)
and nothing widens one into the other. The planner is handed a `Reader`, the
applier a `Writer`, so the read-only path stays read-only by the type it holds
rather than by whether the code remembers not to call something.

`plan` and `apply` are the same decision. `plan` is `apply` with the third phase
removed, not a separate code path, so what you review is what runs.

### Partial failure

`Apply` does not abort the rule when a batch fails. One folder rejecting a write
is no reason to leave the other forty untouched.

That makes the returned state the important part. `Outcome.Next` describes the
world in which every action succeeded, so persisting it after a partial failure
would record the two sides as agreeing on a secret that was never written, and
the next pass would see no drift and never retry. Each action therefore carries
the state entry as it was *before* the run, and `Apply` puts back exactly the
entries whose writes did not land. An entry that did not exist before the run is
removed rather than left at its planned value: "never seen" and "seen and
agreed" are different answers to the next pass's question, and only the first is
true.

Batches are ordered deterministically (side, then folder, then create before
update before delete), so two runs over the same plan issue the same requests in
the same order. That is what makes a partial failure reproducible.

## The state file

The state file records what the mirror last saw on each side of every rule. It
is what tells "created over there" apart from "deleted over here": without it,
a key present on one side and absent on the other is ambiguous, and the mirror
would either resurrect deletions or propagate them, with no way to choose.

### It never holds a value

Entries hold keyed hashes, never values, so the file says whether two sides
agree without saying what they agree on.

The hash is bound to the entry key:

```
HMAC(salt, len(entryKey):entryKey || len(value):value || len(comment):comment)
```

Two properties fall out of that shape.

**Key binding.** Without it, two secrets holding the same value would hash
identically, and the state file would be a map of which credentials are reused
across which folders and environments. That map is worth more to an attacker
than any single hash.

**Length prefixes.** Without them, `("ab", "c")` and `("a", "bc")` would hash
alike, so a value could be moved across a field boundary without the state
noticing.

### The salt

`saltEnv` names an environment variable holding the salt. Left empty, the store
generates one on first use and keeps it in the state file. That is simpler, and
it is strictly weaker: a self-contained file means whoever holds it can test a
guessed value against a hash offline. Naming a variable splits the two, so the
file alone says nothing.

The minimum length is 16 bytes. A short salt is worse than an obviously absent
one, because it looks like protection.

Surrounding whitespace is trimmed off the variable's value. A salt differing
only by a trailing newline, off a shell or a mounted file, would void every hash
recorded under it, invisibly.

### Tombstones

A key deleted on both sides is kept as a tombstone rather than dropped, so a
later pass does not read the absence as "new over there" and resurrect the
secret. The hashes are cleared, so a resurrected key carrying the old value does
not look converged, and no hash of a retired secret outlives the secret.

Tombstones are pruned once they are older than the cutoff. One only has to
outlive the chance of the other side still holding the key; keeping them forever
grows the file without bound.

### Locking

The file backend takes an advisory lock and **refuses to run without one**,
rather than running unlocked. Two concurrent passes would each write back a
document missing the other's work, which surfaces later as unsynced secrets a
long way from the cause.

The lock is non-blocking. A writer that cannot take it stops rather than waits,
because the other holder is a mirror pass over the same secrets and queueing
behind it only moves the collision.

Release builds target Linux and macOS. The `unix` build constraint would also
match solaris and aix, which have no `syscall.Flock`, so the platform list is
spelled out and `lock_other.go` fails with an explanation rather than a missing
symbol.

## Guards

Two refusals stand between a plan and two live instances.

### Empty side

If state tracks keys for a rule and one side now lists nothing, the run stops.

An expired token, a revoked permission and a genuinely emptied folder all answer
a list call the same way, and one of those three is a mass deletion. Only state
can tell them apart, and only by refusing.

This guard has **no floor and must not acquire one**. A side reading empty is not
a proportion of anything: one tracked key vanishing is the same signal as a
thousand, and it matters most on a small rule, whose whole scope fits inside the
floor the ratio guard uses.

### Change ratio

If the destructive actions (updates and deletes; creates are not destructive)
exceed `maxChangeRatio` of the tracked keys, the run stops.

The denominator is the keys this rule *already syncs and still selects*. On a
first run nothing is tracked, so the keys in play stand in; without that, a first
run against two populated sides could overwrite everything with no ratio to
measure it against. Tombstones are excluded: counting last month's deletions into
the share hides today's.

`minDestructiveToGuard = 3` is the floor under which this guard does not fire,
whatever the ratio works out to. A ratio alone cannot tell a small rule from a
runaway one. Rotating one secret in a folder of three is 33%, over any sane
limit, so without a floor the guard blocks the most ordinary operation there is
on every small rule. The way operators make that stop is to set `maxChangeRatio`
to 1, which removes the guard everywhere, including from the rules it was meant
for. Two overwrites that slip past the floor are two lines in a plan a human is
reading; the guard exists for the plan nobody can read.

### Getting past a guard

`--force` exists on `apply` and deliberately not on `plan`. `plan` writes
nothing, so its exit 3 is a report, and a flag to silence it would only ever be
used to stop reading it. On `apply` the guard stands between a decision and two
live instances, so there has to be a way past, and it has to be an explicit act
that shows up in a command line and in a log.

### What the guards do not cover

The guards are about *proportion* and *absence*, not about a first sync. On a
rule that has never run, creates are not destructive, so a large first pass is
invisible to the ratio guard, and the empty-side guard keys on tracked state that
a first `plan` has never written. Stage a first cutover with `dryRun` on the rule
rather than relying on the guards to catch it.

## Config validation

### No two writers on one folder

`validateNoOverlappingWrites` rejects two rules that would write into the same
folder tree. Two writers on one folder fight, and the loser is a secret.

Handing a subtree from a wide rule to a narrow one is allowed, but it has to be
said out loud: the wide rule must exclude that folder and everything under it.
Include patterns are deliberately **not** consulted. A narrow include list also
keeps a pair out of a folder, but relying on that to separate two writers is
fragile, and the conservative answer costs only a config error naming the exact
exclude to add.

### No rule that feeds itself

`validateNoSelfOverlap` rejects a rule whose two sides sit on the same instance,
project and environment with one path inside the other.

Such a rule feeds itself. With `a: /mirror` and `b: /mirror/sub`, the first pass
copies `/mirror/x` to `/mirror/sub/x`; the second pass reads `/mirror/sub/x` as a
new secret under the source root and copies it to `/mirror/sub/sub/x`, and so on,
one level deeper every run.

**Neither runtime guard catches this.** Every action is a create, so the
change-ratio guard sees nothing destructive, and no side is ever empty. Config is
the only place it can be stopped.

Equal paths are the degenerate case and are rejected with the same message: a
rule syncing a folder to itself has no work to do that is not a loop.

### Durations

`interval: 15m` is the only accepted form. yaml.v3 has no built-in
`time.Duration` support: it decodes the shorthand as a type error and a bare
number as nanoseconds, so `interval: 15` would mean fifteen nanoseconds and the
daemon would reconcile in a hot loop against two live instances.

The YAML tag is checked as well as the parse. yaml.v3 will happily decode the
scalar `0` into a string, and `time.ParseDuration` accepts `"0"` without a unit,
so `interval: 0` would otherwise parse cleanly as a zero duration,
indistinguishable from the field being absent, and quietly become the default.
Requiring `!!str` rejects `15` and `0` alike, which is the whole class.

The daemon cadence is validated even for a one-shot run. One config is checked
once and used by both modes, and an interval that only fails when somebody
eventually passes `--daemon` is a config that passed validation and is still
wrong.

## Path handling

The reconciler works in rule-relative paths, because that is the only way two
differently rooted trees can be compared. The API works in absolute ones.
`RelativeTo` and `JoinRelative` are the two directions of that conversion.

Getting the direction wrong writes a secret into the wrong folder, and on a
folder-scoped permission model that is a secret with a different audience.

`RelativeTo` returns a second value reporting containment, and that is the point
of it. **Trimming a string prefix is not the same as trimming a path prefix, and
the difference is silent**: `/apps/x` trimmed by the prefix `/app` yields `/s/x`,
which is a plausible-looking folder that the pair's own include and exclude
patterns will then happily match. This used to be documented as an assumption the
caller had to establish, and the caller that mattered, the planner, took paths
straight from a server response and established nothing.

The planner therefore rejects any listed secret outside the requested subtree.
The listing asked for one subtree, so a row from outside it is the server
answering a different question: an import that came back despite
`includeImports=false`, or a path this build does not understand.

The state key is `<relPath>|<secretKey>`. The separator is safe on the left
because Infisical restricts folder names to letters, digits, dashes and
underscores; a secret key can hold anything, so the split takes only the first
separator.

## Normalisation

The server rewrites values and comments on the way in. Anything it rewrites that
the mirror does not makes the next comparison differ, so the same secret is
rewritten on every pass, forever.

**Values.** `NormalizeValue` applies JavaScript's `String.trim()` on both ends,
keeping a single trailing newline if the value had one, and it is applied on read
as well as on write. Go's `TrimSpace` and JavaScript's `trim` disagree on a few
exotic code points (U+FEFF among them), so a value ending in one still
oscillates. That is deliberate: matching the common case exactly is worth more
than a hand-rolled Unicode table that drifts from whatever V8 does next.

**Comments.** The asymmetry that forces this is in the API's own schema. Creating
a secret declares `secretComment` as a trimmed string, so surrounding whitespace
is removed server-side; updating one declares it untrimmed and stores what it is
given. A padded comment written by a create comes back trimmed and no longer
matches what was recorded. Unlike a value, a comment keeps no trailing newline:
the server's trim takes it, and nothing reads a comment for its exact bytes.

**`secretComment` is sent on every write, including when empty.** On update, an
absent `secretComment` leaves the destination's existing one in place, so a
comment could be set and changed but never cleared, while the reconciler recorded
the source's hash for both sides as though they had converged. The next pass
would see the same difference and rewrite the same secret, forever.

## Talking to the API

### Query parameters switched off

`ListSecrets` turns off three parameters that default to true server-side:

- `expandSecretReferences` would materialise a `${OTHER}` reference into the
  literal it points at, so mirroring the result would replace a reference with a
  copy and silently break the indirection.
- `includeImports` would return another folder's secrets as if they were this
  folder's, so mirroring would turn a link into duplicated real secrets on the
  far side.
- `includePersonalOverrides` would return one user's private override in place of
  the shared value.

### Truncated listings

A truncated listing is indistinguishable from a folder whose secrets were all
deleted, and under a propagate-deletes policy the mirror would act on the
difference. Nothing in the response says how many there were in total, so the
only available check is to ask for a bound and refuse a reply that reaches it.

The request is the weak half of this: an unrecognised query parameter is ignored
rather than rejected, so on a server that does not implement it this catches
nothing, and the empty-side and change-ratio guards are what remain.

### `secretValueHidden` is a pointer

A pointer so that "the server said false" and "the server did not say" are
distinguishable. Go's zero value for a bool is the unsafe answer here: an
unreadable secret arrives with an empty value, and if the field that flags it is
renamed, dropped by a proxy, or absent on an older instance, a plain bool would
decode as `false` and the blank would be mirrored over a real secret.

### Folders are created explicitly

Infisical does not create a destination folder implicitly. Writing a secret into
a folder that does not exist fails with `Folder with path ... not found`, so the
mirror builds the tree itself, parents first.

### Redirects are never followed

Go strips the `Authorization` header across a redirect only when the hostname
changes; it never looks at the scheme. So an `https` URL redirecting to `http` on
the same host re-sends the token, and carries the reply back, in the clear. An
API has no business redirecting us anywhere, so none are followed.

### `AuthError` does not unwrap

The login is an ordinary request through this same client. Without a barrier,
`errors.As` would walk straight through an auth failure: a 404 from a mistyped
instance URL would satisfy `IsNotFound` and be reported as a missing project, and
a 409 would satisfy `IsConflict`, which `EnsureFolder` reads as "the folder is
already there" and carries on past a folder that was never created.

It is also terminal for the retry loop. The login does its own retrying, and a
loop inside a loop turns four attempts into sixteen POSTs against the one
endpoint that is most aggressively rate limited.

A token that arrives already inside the renewal skew is a hard error rather than
something to work around. It can never satisfy the cache check, so every
subsequent request would log in again: a silent storm against that same endpoint.
The likely causes are a field that moved and a unit that is not seconds, and both
are worth stopping for.

## The webhook trigger

A webhook only ever changes *when* a pass runs, never what it does. The pass
it starts is the same function the timer starts, over every rule, so nothing a
caller sends can narrow, widen or steer a reconcile.

### Verifying a request

Infisical signs with HMAC-SHA256 over `JSON.stringify(payload)` and sends
`x-infisical-signature: t=<unix ms>;<hex>`, with the same timestamp as a field
inside the payload (`triggerWebhookRequest` in
`backend/src/services/webhook/webhook-fns.ts`). Three consequences:

- The hash is taken over the raw bytes received, never over a re-encoding.
  Go's encoder escapes `<`, `>` and `&` and sorts map keys, so a decode and
  re-encode changes the bytes of, say, any project name with an ampersand. The
  test fixture's body and signature were produced outside Go for that reason.
- The header's `t=` is not covered by the signature; the body's `timestamp` is.
  The two must match, and the signed one is what the five-minute skew check
  reads. That window is how long a captured request can be replayed. A replay
  cannot change what a pass does, only how often passes run: replayed in a
  loop, it keeps them back to back, one per pass duration plus debounce, for up
  to five minutes. There is no nonce cache; the debounce is the rate limit.
- The header grammar is accepted exactly and nothing looser: lowercase hex, no
  `v1=`, no extra fields. Infisical's own PKI alerts use a different,
  Stripe-style header, and a lenient parser is how a second format starts being
  accepted by accident.

The handler answers every request with a bare status code and no body; only
the mux's own 404 and 405, for paths and methods outside the one route, carry
Go's default text. An unknown instance gets 404 and a known one with a bad
signature 401, which tells a caller which names exist; instance names are
not secret, and the distinction is what makes a misconfigured URL
diagnosable. `changedBy` is never decoded,
because for a user it is an email address.

### Scheduling

The loop is one goroutine and runs passes inline, so overlap is impossible by
construction rather than prevented by a lock. Events reach it through a
channel of capacity one written without blocking, which collapses any number
of events into one pending trigger, and an event that arrives mid-pass waits
there and produces exactly one follow-up.

A trigger only moves the next pass *earlier*, to `now + debounce`, and never
later. A sliding debounce, where each event restarts the wait, would let a
steady stream of edits postpone the pass indefinitely. With this rule a stream
produces one pass per `pass duration + debounce` at most, and
`TestSchedulerSteadyStreamDoesNotStarve` fails against the sliding version.

The timer is not replaced. Infisical's request client retries a delivery up
to three times on a 429, a 5xx or a network error (axios-retry in
`backend/src/lib/config/request.ts`), and never on any other 4xx or on a
timeout; after that the failure is recorded and dropped. A retry carries the
same bytes and timestamp, so it verifies and coalesces like any duplicate. An
event that is lost anyway is caught by the next interval and nothing else.

### What is not done

The event names a project, environment and folder, so a pass could be limited
to the rules that match. It is not: a pass over one folder would feed the
change ratio guard that folder's keys as its denominator, so a bulk edit the
guard allows across a whole rule could be refused in a small folder, and the
delete and empty-side guards would need the same re-examination against a
partial listing. Echoes are not filtered either. The mirror's own
writes make the other side send an event, and the only way to recognise one is
`changedBy`, which is a display name rather than an id. The cost is one pass
that writes nothing, which sends nothing, so the chain stops there.

## Keeping secrets out of output

### Every credential-bearing type defines `String` and `GoString`

An unexported field is **not** a redaction mechanism. `fmt` reaches unexported
fields by reflection, so `%+v` prints one, and `%#v` prints one even past a
`String` method. Only the pair of `String` and `GoString` covers every verb, and
both are needed on every type that holds a credential or a secret value.

`reconcile.Action` holds its value unexported *and* defines `String`, because a
plan reaches a terminal and CI logs, and an exported value field would reach both
through a single `%+v`.

`TestNothingPrintsASecretValue` is the regression test for that whole surface
rather than for one type: every struct holding a secret value is printed through
every verb, alone and nested inside the container that carries it in real code (a
slice of secrets, a `WriteRequest` holding a batch, the client holding a token
source). One missing `String` method anywhere in that graph puts the value into
whatever the caller logged.

### Error bodies are never quoted raw

`errorMessage` pulls the server's own message out of an error body and never
quotes an unrecognised one.

An earlier version fell back to the raw text, on the reasoning that this reads
the *response* and not the request, so nothing secret could reach it. That covers
one side of the wire only: a WAF, a proxy or a validation layer that quotes the
payload it rejected hands the request straight back, and for the login endpoint
the request body is the machine identity's client secret. Probing a server that
echoes its input put exactly that on stderr.

So the raw fallback is gone, the parsed message is clamped, and on the two
endpoints whose bodies carry credentials or secret values nothing is quoted at
all. An unparseable body is reported by size, which is enough to tell a proxy's
HTML error page from the API's JSON one.
