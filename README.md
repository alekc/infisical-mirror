# infisical-mirror

Keeps two Infisical instances in step: recursively over the folder tree, across
several environments, one way or both ways per rule.

Infisical's own instance-to-instance secret sync targets a single folder in a
single environment, flattens subfolders into it, and offers the reverse
direction only as a one-shot import. External Secrets Operator loses the source
folder before a `PushSecret` can see it
([external-secrets#6873](https://github.com/external-secrets/external-secrets/issues/6873)).
This fills that gap.

**Status: v0.2.0.** It has been mirroring one real pair of instances in `apply`
mode since 2026-09-20, and that is the whole of its production record. The
webhook trigger is new in v0.2.0 and has no production record yet. The config
surface may still change before 1.0. Start with `plan`, then `dryRun: true`,
then widen.

- [Install](#install)
- [Quick start](#quick-start)
- [Commands](#commands)
- [Configuration](#configuration)
- [Deploying it](#deploying-it)
- [How a key is decided](#how-a-key-is-decided)
- [Safety](#safety)
- [Metrics](#metrics)
- [Development](#development)

## Install

Binaries and checksums are attached to each
[release](https://github.com/alekc/infisical-mirror/releases), for linux and
darwin on amd64 and arm64:

```sh
VERSION=0.2.0
curl -fsSL -o infisical-mirror.tar.gz \
  "https://github.com/alekc/infisical-mirror/releases/download/v${VERSION}/infisical-mirror_${VERSION}_linux_amd64.tar.gz"
tar -xzf infisical-mirror.tar.gz infisical-mirror
install -m 0755 infisical-mirror /usr/local/bin/
```

Container image, `linux/amd64` and `linux/arm64`:

```sh
docker pull ghcr.io/alekc/infisical-mirror:0.2.0
```

Debian and Ubuntu, from the `.deb` attached to each release (amd64 and arm64,
from v0.1.1 onward):

```sh
VERSION=0.2.0
curl -fsSLO "https://github.com/alekc/infisical-mirror/releases/download/v${VERSION}/infisical-mirror_${VERSION}_linux_amd64.deb"
sudo dpkg -i "infisical-mirror_${VERSION}_linux_amd64.deb"
```

The package brings a systemd unit and a default config, and **deliberately does
not enable the service**: the shipped config carries placeholder project slugs
and `dryRun: true`, so it fails at startup until you fill it in. See
[Deploying it](#deploying-it) for the three steps that follow.

Or from source, which needs Go 1.27 or newer. A `go install` build carries no
link-time flags, so `version` recovers what it can from the build info the
toolchain embeds: the tag when you install one, and the commit when you build
from a checkout. A build from a modified tree says so, and reports `dev` rather
than a version that looks like a release nobody can fetch.

```sh
go install github.com/alekc/infisical-mirror/cmd/infisical-mirror@latest
```

## Quick start

**1. Create a machine identity on each instance** with read access to the
folders you want to mirror, and write access on whichever side is a
destination. Export the credentials; they never go in the config file.

```sh
export CLOUD_CLIENT_ID=... CLOUD_CLIENT_SECRET=...
export SELFHOSTED_CLIENT_ID=... SELFHOSTED_CLIENT_SECRET=...
```

**2. Write a config.** This is the whole of
[examples/config-minimal.yaml](examples/config-minimal.yaml): one folder tree,
one environment, one way.

```yaml
instances:
  cloud:
    url: https://app.infisical.com
    auth:
      universalAuth:
        clientIdEnv: CLOUD_CLIENT_ID
        clientSecretEnv: CLOUD_CLIENT_SECRET
  selfhosted:
    url: https://secrets.example.com
    auth:
      universalAuth:
        clientIdEnv: SELFHOSTED_CLIENT_ID
        clientSecretEnv: SELFHOSTED_CLIENT_SECRET

state:
  backend: file
  file:
    path: /var/lib/infisical-mirror/state.json

rules:
  - name: home-cluster
    mode: a-to-b
    a:
      instance: cloud
      project: my-cloud-project # the slug from the URL, not the id
      path: /
    b:
      instance: selfhosted
      project: my-selfhosted-project
      path: /
    envMap:
      prod: prod
```

**3. See what it would do.** `plan` writes nothing, anywhere, including to the
state file.

```console
$ infisical-mirror plan --config config.yaml
rule arr-stack  prod <-> prod
  a  cloud:cloud-proj:prod:/arr-stack  3 secret(s) in scope
  b  selfhosted:self-proj:prod:/apps/arr-stack  2 secret(s) in scope
  state: first run for this pair, so no key can be told from a new one

  create  b  /  PROWLARR_API_KEY  new on a

  1 to create, 2 converged
```

Every row is `<operation> <side written> <folder> <key> <why>`. No value is
ever printed.

**4. Carry it out.**

```console
$ infisical-mirror apply --config config.yaml
rule arr-stack  prod <-> prod
  a  cloud:cloud-proj:prod:/arr-stack  3 secret(s) in scope
  b  selfhosted:self-proj:prod:/apps/arr-stack  2 secret(s) in scope
  state: first run for this pair, so no key can be told from a new one

  create  b  /  PROWLARR_API_KEY  new on a

  1 to create, 2 converged

  applied: 1 created, 0 updated, 0 deleted
```

A second pass now has state to compare against, and nothing to do:

```console
$ infisical-mirror plan --config config.yaml
rule arr-stack  prod <-> prod
  a  cloud:cloud-proj:prod:/arr-stack  3 secret(s) in scope
  b  selfhosted:self-proj:prod:/apps/arr-stack  3 secret(s) in scope
  state: 3 key(s) tracked
  nothing to do
  3 converged
```

### Easing into it

The order that costs least if something is wrong:

1. `plan` only. Read every row.
2. Set `dryRun: true` on the rule and run `apply` on a schedule. It plans,
   reports, and writes nothing, so you get the shadow signal without the risk.
3. Drop `dryRun` on one rule at a time.

`--dry-run` on the command line forces dry run on everywhere and can never turn
it off, so the flag can only ever make a run safer.

## Commands

```
infisical-mirror plan   --config config.yaml [--rule NAME]... [--exit-code] [--dry-run] [--daemon]
infisical-mirror apply  --config config.yaml [--rule NAME]... [--dry-run] [--force] [--daemon]
infisical-mirror state  show  --config config.yaml [--scope NAME]
infisical-mirror state  prune --config config.yaml [--older-than 720h] [--dry-run]
infisical-mirror version
```

| Flag | Applies to | Effect |
| --- | --- | --- |
| `--config` | all | Path to the YAML config. Required. |
| `--rule NAME` | `plan`, `apply` | Restrict to one rule. Repeatable. |
| `--exit-code` | `plan` | Exit `2` when there are pending changes, for CI. |
| `--dry-run` | `plan`, `apply` | Force dry run on every rule. |
| `--force` | `apply` | Carry out a rule whose guard fired. Per invocation. |
| `--daemon` | `plan`, `apply` | Stay up, repeat every `daemon.interval`, serve metrics. |

| Exit code | Meaning |
| --- | --- |
| `0` | Nothing to do. |
| `1` | The run did not complete. |
| `2` | Pending changes. Only with `--exit-code`. |
| `3` | A guard refused a rule. |
| `64` | Bad usage. |

A rule that cannot be read is reported and the remaining rules are still
planned, so one unreachable instance does not hide the rest.

`plan` structurally cannot write, and not by convention: it is handed a value
implementing only the two read calls, which wraps the client rather than being
it, so there is no write method to reach even by type assertion. `apply` plans
through that same read-only value and writes only afterwards, so the decision is
always made by something that cannot act on it.

`state show` reports what the mirror remembers, without disclosing any of it.
Hashes are truncated, because a full keyed hash is an equality oracle for
anyone who can compute one:

```console
$ infisical-mirror state show --config config.yaml
state  /var/lib/infisical-mirror/state.json
salt   fingerprint d45d1716
       generated and stored in the file itself

SCOPE                                                       LIVE  TOMBSTONED  UPDATED
arr-stack|cloud:cloud-proj:prod:/arr-stack|selfhosted:...    3     0           2026-09-20T11:59:38Z
```

`version` prints the version, commit and build date, which `make build` and the
release both stamp in through ldflags.

## Configuration

Full annotated example:
[examples/config.yaml](examples/config.yaml). Smallest working example:
[examples/config-minimal.yaml](examples/config-minimal.yaml). Unknown fields are
rejected, so a typo is an error at load rather than a setting that silently did
nothing.

### Credentials

Credentials are never read from the config file. The `auth` block names the
environment variables to read them from, and exactly one method per instance,
because which credential a process authenticated with should be readable off
the config:

```yaml
instances:
  cloud:
    url: https://app.infisical.com
    auth:
      universalAuth: # a machine identity
        clientIdEnv: CLOUD_CLIENT_ID
        clientSecretEnv: CLOUD_CLIENT_SECRET

  selfhosted:
    url: https://secrets.example.com
    auth:
      token: # or a token something else already obtained
        tokenEnv: SELFHOSTED_TOKEN
```

An instance URL must be `https`, unless its host is loopback. Every request
carries a bearer token and every reply carries secret values, and Go's HTTP
client strips the `Authorization` header across a redirect only when the
hostname changes, never on a scheme downgrade. For the same reason no redirect
is followed at all.

`project` names the project slug from the Infisical URL rather than the project
id. It is resolved once per run, which is also where a misspelled environment is
caught: an environment that does not exist answers like an empty one, and an
empty environment is what a sync reads as "everything here was deleted".

### Rules

A rule is a named pair of (instance, project, path) endpoints plus an
environment map. Each one has its own direction, folders, environments and
policies:

```yaml
rules:
  - name: home-cluster
    mode: bidirectional # a-to-b | b-to-a | bidirectional
    a:
      instance: cloud
      project: example-cloud-project
      path: /
    b:
      instance: selfhosted
      project: example-selfhosted-project
      path: /
    envMap:
      prod: prod
      dev: dev
```

**Folders map independently on each side**, so `/arr-stack` on one instance can
land on `/apps/arr-stack` on the other, subfolders and all. **Environments map
explicitly**, one reconcile pass per pair, and a pair can override both roots:

```yaml
  - name: arr-stack
    mode: a-to-b
    a: { instance: cloud, project: example-cloud-project, path: /arr-stack }
    b: { instance: selfhosted, project: example-selfhosted-project, path: /apps/arr-stack }
    recursive: false # stay in this folder, do not descend
    dryRun: true # report what it would do, change nothing
    envMap:
      prod: prod
      dev:
        env: staging # dev on side a is staging on side b
        aPath: /arr-stack-dev # and both roots move for this pair only
        bPath: /apps/arr-stack-staging
```

**Include and exclude** narrow a rule within its root, as doublestar globs
against the rule-relative folder path. An empty `include` means every folder;
`exclude` always wins:

```yaml
    include:
      - /apps/** # only these folders, whatever else is under the root
      - /shared/**
    exclude:
      - /scratch/**
      # A trailing /** covers the folder itself as well as everything under it.
      - /apps/arr-stack/**
```

Two rules that would write into the same folder tree are refused at load unless
the wider one explicitly excludes that subtree, because two writers on one
folder fight and the loser is a secret. A single rule whose own two sides
overlap is refused for the same reason: with `a: /mirror` and `b: /mirror/sub`
the rule copies its own output back into its source and nests one folder deeper
every run, and no runtime guard catches it.

### Defaults

Anything in `defaults` applies to a rule that does not set the same field:

```yaml
defaults:
  conflict: newest-wins # a-wins | b-wins | newest-wins | fail
  delete: ignore # ignore | propagate
  maxChangeRatio: 0.25 # abort a rule that wants to change more than this
  recursive: true
  dryRun: false # --dry-run forces this on, and never off
```

### State

The state file is what tells "created over there" apart from "deleted over
here". It holds keyed hashes, never values, so it says whether two sides agree
without saying what they agree on.

```yaml
state:
  backend: file
  saltEnv: MIRROR_STATE_SALT # optional, and recommended
  file:
    path: /var/lib/infisical-mirror/state.json
```

Generate the salt once and store it wherever the instance credentials already
live:

```sh
openssl rand -base64 32
```

Unset, the store generates one on first use and keeps it in the file, which is
simpler but leaves the file self-contained: whoever holds it can test a guessed
value against a stored hash. Supplying the salt separately splits those apart.
The salt is then not recoverable from the state, so the store refuses to open a
file written under a different one rather than silently rehashing everything.
That makes losing it a real cost.

### Daemon and metrics

```yaml
daemon:
  # Only read under --daemon. A unit is mandatory: a bare 15 would decode as
  # fifteen nanoseconds. Minimum 1m, default 15m.
  interval: 15m

metrics:
  listen: :9090 # daemon: GET /metrics and GET /healthz
  textfile: /var/lib/node_exporter/textfile/infisical-mirror.prom # one-shot
```

`metrics.textfile` must be absolute and end in `.prom`, because the
node_exporter collector reads only `*.prom` and ignores anything else in the
directory without saying so. The file is written to a temp file in the same
directory and renamed over the target: a collector scrapes the whole directory,
so a half-written or leftover file becomes a second, stale copy of every series
rather than being skipped.

### Webhook

A daemon waits up to `daemon.interval` to notice a change. With a webhook
receiver it starts a pass a few seconds after Infisical reports one instead:

```yaml
instances:
  cloud:
    url: https://app.infisical.com
    auth: { ... }
    # Names the variable holding the secret key of this instance's webhooks.
    webhookSecretEnv: CLOUD_WEBHOOK_SECRET

webhook:
  listen: :9091 # off unless set; its own port, so /metrics stays internal
  debounce: 10s # default 10s, minimum 1s, at most daemon.interval
```

Each instance with a `webhookSecretEnv` gets the route
`POST /webhook/<instance name>` on `webhook.listen`. In that instance, under
Project Settings > Webhooks, add a webhook of type General pointing at it, set
its secret key to the value of the named variable, filter it to the Secret
Modified event, and scope it to the environments and paths your rules cover.
The secret path is a glob, and `/` matches only the root folder, whatever the
form's hint says: use `/**` to cover every folder. The Test button should get
a 200 back; it is checked like a real event and starts nothing.

What the receiver does with a request:

- Anything without a valid `x-infisical-signature` gets a 401, and so does a
  signed event whose timestamp is more than five minutes from the local clock.
- A `secrets.modified` event schedules a pass `debounce` after it arrives. More
  events before then join the same pass, events during a pass give exactly one
  more after it, and two passes never run at once.
- The event names a folder, not a secret, so the pass is the ordinary full
  pass: every rule is listed, and only what differs is written.
- The interval keeps running. Infisical retries a delivery a few times on a
  5xx or a network error and then gives up, so the timer is what catches a
  missed one.

Two things to know before relying on it. Infisical will not post to a private
or internal address unless the sending instance sets
`ALLOW_INTERNAL_IP_CONNECTIONS`, so Infisical Cloud needs a URL it can reach
from the internet. And the mirror's own writes are changes too: a pass that
writes to one side makes that side send an event, which costs one extra pass
that finds nothing to do.

The config is refused when only half of it is there: a listener with no
instance holding a secret, a `webhookSecretEnv` with no listener, or a listener
on the metrics address. A named variable that is empty stops the daemon at
startup. A one-shot run validates the block, since one config serves both
modes, but starts no receiver, having no process left to receive anything.

## Deploying it

Three shapes, one binary.

**One-shot** is the CronJob shape: run it, it does one pass and exits. With
`metrics.textfile` set it writes a Prometheus textfile for the node_exporter
collector on the way out; without it, no metrics at all, because there is nobody
to scrape a process that has ended.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: infisical-mirror
spec:
  schedule: "*/15 * * * *"
  concurrencyPolicy: Forbid # a second pass would wait on the state lock anyway
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: OnFailure
          containers:
            - name: mirror
              image: ghcr.io/alekc/infisical-mirror:0.2.0
              args: [apply, --config, /etc/infisical-mirror/config.yaml]
              envFrom:
                - secretRef:
                    name: infisical-mirror-credentials
              volumeMounts:
                - { name: config, mountPath: /etc/infisical-mirror }
                - { name: state, mountPath: /var/lib/infisical-mirror }
          volumes:
            - name: config
              configMap: { name: infisical-mirror-config }
            - name: state
              persistentVolumeClaim: { claimName: infisical-mirror-state }
```

**`--daemon`** stays up, repeats the pass every `daemon.interval`, and serves
`GET /metrics` and `GET /healthz` on `metrics.listen`. The interval is measured
from the end of a pass, so a slow pass delays the next one instead of
overlapping it, and `metrics.textfile` is ignored. Use it when you want a live
scrape target rather than a textfile.

Either way **the state file has to outlive the container**. Mount a volume at
`/var/lib/infisical-mirror`; without one, every start is a first run and every
key looks new on both sides.

**The Debian package** is the third shape, and it is `--daemon` on a host
rather than in a cluster. `dpkg -i` installs the unit without enabling it, so
bringing it up is three deliberate steps:

```sh
sudoedit /etc/infisical-mirror/config.yaml   # replace the REPLACE-ME slugs
sudoedit /etc/infisical-mirror/env           # the four credential variables
sudo systemctl enable --now infisical-mirror
```

It will run in dry-run until you clear `defaults.dryRun` in the config, which
is the point: watch a few passes with `journalctl -u infisical-mirror` first.
Both files under `/etc/infisical-mirror` are conffiles, so an upgrade never
overwrites them, and `/etc/infisical-mirror/env` is the one installed at 0640
because it is the one holding credentials.

The unit runs under `DynamicUser=yes` with `StateDirectory=infisical-mirror`,
so systemd owns the account and the state directory and there is no service
user to create or clean up. One consequence worth knowing: `apt purge` removes
`/var/lib/infisical-mirror` along with the config, so the next install starts
with no record of what was reconciled. A plain `apt remove` keeps both.

## How a key is decided

Both sides hold it and the values agree: nothing to do. They disagree, and the
last-synced state says one side changed: that side wins, whatever the conflict
policy says, because there is nothing to arbitrate. Both changed, or there is no
state to compare against: the conflict policy decides, and `newest-wins`
declines to pick when the two update times are equal rather than tossing a coin
over a real secret.

One side has it and the other never did: it is created on the far side. One side
has it and the other used to: that is a deletion, and `delete` decides whether
the surviving copy goes too. A one-way rule's destination is exempt, because a
key deleted there is a missing copy rather than a deletion, and the next pass
restores it from the source.

A deletion on one side against an edit on the other is never resolved
automatically, whatever the two policies say. They disagree about that case and
either answer destroys work somebody did, so it is reported and left alone.

Under `delete: ignore` the state entry is deliberately kept rather than dropped.
Keeping it is what stops the next pass reading the surviving copy as a brand new
secret and putting the deleted key back. A key that is gone from both sides is
tombstoned instead, and a key that reappears after that is treated as new,
because somebody adding a secret back expects it to sync like any other.

## Safety

A two-way sync's failure mode is deleting real data, so:

- **A side that returns no secrets where state holds entries aborts that rule**,
  because an expired token, a revoked permission and an emptied folder look
  identical in the response. This guard has no floor and must not acquire one.
- **A run that would overwrite or delete more than `maxChangeRatio` of a rule's
  tracked keys aborts.** Creating a key destroys nothing and is not counted, so
  a first run does not trip it. A first run has nothing tracked to measure
  against either, so the keys in play are the denominator instead. The ratio has
  a floor of three destructive actions, below which it does not fire whatever
  the percentage works out to.
- **`plan` has no override**, because a flag to silence an alarm on a command
  that writes nothing would only ever be used to stop reading the alarm.
  `apply --force` is the override, and it is per invocation rather than a config
  setting, so nothing makes it the standing default.
- **A listing is requested with an explicit bound and refused if it reaches it.**
  A truncated reply is indistinguishable from a folder whose secrets were all
  deleted, and nothing in the response says how many there were in total.
- **Secret references are never expanded**, so a `${FOO}` stays a reference
  instead of being materialised into a literal on the far side, and **imported
  folders are not followed**, so a link stays a link.
- **A value the credential cannot read fails the run** rather than being
  mirrored as a blank. It arrives as an empty string, indistinguishable from an
  empty secret, and the same applies when the server sends no
  `secretValueHidden` field at all.
- **Values never reach a log line.** Every type holding one renders itself
  without it, through both `String` and `GoString`, and carries `json:"-"` so it
  cannot be serialised out either. A server's error body is never echoed back.
- **A failed state write leaves the previous file byte for byte intact.** The
  document is written to a temp file in the same directory, fsynced, renamed
  over the target, and the directory fsynced after the rename.
- **A failed batch does not record its keys as synced.** Each action carries the
  state entry it is replacing, and a batch that does not land puts those entries
  back, so the next pass still sees the drift and retries it.
- **`plan` takes a shared lock, `apply` an exclusive one.** An operator asking
  what the scheduled pass is about to do is not refused by the scheduled pass
  itself; two plans can run at once. A second `apply` waits rather than
  interleaving its writes, and under `--daemon` that is one lock for the life of
  the daemon rather than one per pass.
- **No secret key, value or folder path ever becomes a Prometheus label.** A
  test asserts the whole label vocabulary rather than only checking for
  known-bad strings.

Why each of these is shaped the way it is: [docs/design.md](docs/design.md).

## Metrics

Everything is under the `infisical_mirror_` prefix. Per-rule series carry
`rule`, `env_a` and `env_b`, and nothing else identifying: no key, no value, no
folder path, ever.

| Metric | Type | Extra labels |
| --- | --- | --- |
| `build_info` | gauge | `version`, `commit`, `date` |
| `last_run_timestamp_seconds` | gauge | `command` |
| `last_success_timestamp_seconds` | gauge | `command` |
| `last_run_duration_seconds` | gauge | `command` |
| `runs_total` | counter | `command`, `result` |
| `rule_secrets` | gauge | `side` |
| `rule_actions` | gauge | `op` |
| `rule_converged` | gauge | |
| `rule_tracked_keys` | gauge | |
| `rule_blocked` | gauge | |
| `rule_failed_actions` | gauge | |
| `rule_errors` | gauge | |
| `applied_total` | counter | `op` |
| `passes_started_total` | counter | `trigger` (`timer` or `webhook`), daemon only |
| `webhook_requests_total` | counter | `source`, `outcome` |

`webhook_requests_total` labels a request with the configured instance it
named, or an empty `source` when it named none, so a caller cannot create series
by inventing paths. The label is `source` rather than `instance` because
Prometheus already sets `instance` on every scraped series. A rising
`bad_signature` or `stale` count is a secret or a clock that disagrees with
Infisical.

The per-rule gauges are cleared at the start of every pass, so a rule dropped
from the config stops reporting rather than holding its last values forever; a
stale gauge reads as a rule that is fine. The counters are never cleared, since
a counter going backwards makes every `rate()` over it misread.

An op that had nothing to do reports zero rather than being absent. On a graph
an absent series and a zero one look identical and mean opposite things.

Three alerts worth having:

```promql
# The mirror has not completed a pass in an hour.
time() - infisical_mirror_last_success_timestamp_seconds > 3600

# A guard is refusing a rule, so nothing is syncing on it.
infisical_mirror_rule_blocked > 0

# Writes are failing on a rule that is otherwise running.
infisical_mirror_rule_failed_actions > 0
```

`last_success_timestamp_seconds` moves only on a successful pass, which is what
makes the first one a staleness alert rather than a liveness one.

## Development

```sh
make               # fmt, vet, lint, test
make test-race     # the one that matters: one client is shared across goroutines
make lint          # golangci-lint, config in .golangci.yml
make build         # bin/infisical-mirror, version stamped from git
make version-check # proves the ldflags still reach the binary
```

[docs/design.md](docs/design.md) holds the reasoning behind the state file, the
guards, the config validation and the redaction rules. The code comments state
the rule; that file says why it is that rule.

A release is a pushed `v*` tag: goreleaser builds the archives, the checksums
and the multi-platform image. `goreleaser check` runs in CI, because a release
that first discovers its config is invalid does so after the tag is already
pushed.

## Licence

MIT.
