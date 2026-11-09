# CPA Quota Guard

Per-account quota reserves and automatic use of expiring Codex reset credits for CLIProxyAPI, configured inside CPAMC.

[中文](README.md) · [Installation](docs/INSTALLATION.md) · [Configuration](docs/CONFIGURATION.md) · [Testing](docs/TESTING.md)

Independent community plugin; not affiliated with CLIProxyAPI, CPAMC, OpenAI, or Google.

![CPAMC interface with fictional test accounts](docs/images/light.png)

## Features

- Per-account or per-window minimum remaining quota, a safety margin, and recovery hysteresis.
- Observe/enforce modes, stale quota handling, priority-aware round-robin or fill-first scheduling.
- Codex and Antigravity quota adapters.
- Server-side use of expiring Codex reset credits, configurable lead time and account switches.
- Durable redemption IDs and event history for verification after failures or restarts.
- Existing CPAMC login and theme, without a second plugin login or credentials copied to the iframe.
- Native-style controls and motion, keyboard tabs, responsive layout, and reduced-motion support.
- Independent quota/reset workers, bounded concurrency, retry backoff and retained last-good quota snapshots.
- Live account summaries separated from editable policy drafts, search/filtering, grouped errors and preserved conflict drafts.
- Optional private Webhook notifications, build provenance, compatibility diagnostics and verified rollback.

## Verified compatibility

CLIProxyAPI **8.0.35**, C ABI 1 / JSON schema 6; CPAMC **1.26.5** with the exact verified HTML build. The installer rejects unknown panel hashes before making changes. The packaged library targets Linux amd64 / glibc and is built on Ubuntu 24.04. Other versions and platforms have not been verified.

The Linux installer requires Python 3, PyYAML, and systemd. Source builds require Go 1.23+ and a C compiler; Go 1.27.3 is used for validation.

## Install

Download the archive and `SHA256SUMS` from this repository's Releases:

```bash
sha256sum -c SHA256SUMS
tar -xzf cpa-quota-guard_0.3.0_linux_amd64.tar.gz
cd cpa-quota-guard-0.3.0
sha256sum -c dist/SHA256SUMS
python3 install.py  # read-only plan
python3 install.py --doctor  # independent compatibility/build diagnosis
sudo python3 install.py --apply --management-key-file /root/cpa-management.key
```

The key file must contain your existing CPA management key and be readable only by root. The installer backs up the library, configuration, and panel, restarts CPA, verifies registration, and rolls back on failure. Open **账号额度保护** in CPAMC after signing in normally. See the installation guide for rollback and custom paths.

**0.3.0 first-install defaults: observe quota protection, automatic credit use disabled, and new accounts excluded from automatic redemption.** The switches are independent: observe mode does not suspend reset automation once enabled. Select accounts in CPAMC before enabling, or explicitly use `--auto-reset on --reset-account example-account.json` on first installation. Upgrades preserve existing policy and journal bytes. Older policies without `include_new_accounts` retain the previous `true` behavior; review that switch after upgrading.

Management-only/SSO plugins may coexist. Scheduler capability checks replace the former blanket rejection. CPA 8.0.35 lacks scheduler flags, so audited capability declarations must match the exact installed binary SHA256; unknown capabilities block apply before changes. Existing panel extensions also need exact marked-block SHA256 declarations, while the original verified CPAMC HTML hash remains mandatory. See [installation](docs/INSTALLATION.md).

## Important behavior

Quota protection is best-effort. In-flight requests, other clients, delayed quota data, and host fallback can still spend quota. A protected window pauses the whole account; plugin scheduling replaces native selection for matching enabled rules.

The upstream consume endpoint cannot select a particular credit. If a different credit becomes unavailable, the plugin pauses that account until the target credit expires. Successful redemption depends on upstream availability and account authorization. Do not delete the durable journal while the service is running.

## Build and contribute

```bash
git clone https://github.com/Wuse777/cpa-quota-guard.git
cd cpa-quota-guard
bash build.sh
```

See [CONTRIBUTING.md](CONTRIBUTING.md) and [docs/TESTING.md](docs/TESTING.md). Unit and isolated integration tests use fake accounts; production credentials, logs, and runtime state are not part of this repository.

The build writes `dist/BUILD.json` with version, commit, time, target, ABI/schema and binary SHA256. Installation verifies artifact integrity and running build identity. Both normal PR and release-tag CI gates execute real isolated CPA integration, process and systemd installation/rollback, synthetic browser regression and real CPAMC login/bridge smoke tests using pinned public fixtures.

Notifications are disabled unless `notification_webhook_file` is configured. This service-owned regular 0600 file contains an HTTPS Webhook URL; the URL is never returned in status or errors. Notifications use sanitized event types and account fingerprints, bounded timeouts, deduplication and retry backoff. See [configuration](docs/CONFIGURATION.md) for fields and permissions.

MIT license; upstream and dependency notices are preserved in [NOTICE](NOTICE) and [licenses/](licenses/). Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

## Reset forwarding in 0.2.2

Card queries and redemption use CPA's loopback `/v8/management/requests/api-call`, the same endpoint used by CPAMC's manual reset. CPA resolves the account token. Run the new installer when upgrading: it provisions the loopback management URL and a service-owned 0600 management key file. Replacing only the shared library is insufficient. Failures retain CPA/upstream HTTP status, structured codes and sanitized summaries across successful list queries and restarts; confirmed redemption clears the current error while preserving event history. See the installation and configuration guides for key rotation and rollback.
