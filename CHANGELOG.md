# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - unreleased

First release.

### Added

- Provider plugin that reads router hostnames from Traefik's API (Docker labels,
  file provider and `defaultRule`) and publishes them to Cloudflare.
- Mode per entrypoint (`entryPointModes`, `defaultMode`): `ddns`, `tunnel` or `none`.
- DDNS: `A` records for the public IPv4 address; opt-in `AAAA` (`ddns.ipv6`);
  fallback IP sources or static addresses; proxied by default, DNS-only for
  `ddns.dnsOnlyEntryPoints` and TCP hosts.
- Cloudflare Tunnel: proxied `CNAME` to `<tunnel-id>.cfargotunnel.com` and, for
  remotely-managed tunnels, public-hostname rules merged into the tunnel
  configuration without disturbing other rules or settings.
- Zones found by longest-suffix match across every zone the token can see, with an
  optional allow-list.
- Safety: `exclude` globs, an ownership marker comment on every record, `adopt` to
  take over existing records, and conflict detection.
- Pruning (on by default) of owned records and tunnel rules once a host has been gone
  for `pruneGrace`.
- Opt-in discovery of TCP routers' `HostSNI` names and routers' `tls.domains`.
- `dryRun`, `apiTokenFile`, and config validation with clear errors.
