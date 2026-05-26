# shrimp-basket

A local Go quarantine proxy for PyPI and npmjs that mitigates supply chain attacks by filtering out package versions published within the last 7 days.

## How it works

1. **Always-on User Service**: Runs as a systemd user service listening on `127.0.0.1:12345`.
2. **Metadata Quarantine**: Intercepts requests for package version lists, fetches upstream metadata, and strips any versions published `< 7 days ago`.
3. **No-Latency File-Based Caching**: Caches filtered indices in `~/.cache/shrimp-basket/`.
4. **Direct CDN Downloads**: Returns the safe, filtered metadata containing absolute URLs. Package manager clients download binaries (`.whl`/`.tgz`) directly from the official CDNs (PyPI/NPM), consuming zero local proxy storage.
5. **Daily Cache Update**: A systemd user timer updates metadata lists once a day.

## Quarantine Caveats

* **NPM dist-tags (e.g. `latest`)**: When a package's `latest` version is within the 7-day quarantine window, the proxy removes it from the available versions map but leaves the `latest` tag pointing to it. This causes package managers to fail loudly (e.g., "no matching version found") rather than silently downgrading to an older version.

## Installation

To compile, place systemd unit files, and configure `~/.npmrc`, `~/.config/uv/uv.toml`, and `~/.config/pip/pip.conf` globally:

```bash
make install
```

Verify it running:
```bash
systemctl --user status shrimp-basket.service
```

Read logs:
```bash
journalctl --user -u shrimp-basket -f
```

## Uninstallation

To disable background units and restore original global registry configurations:

```bash
make uninstall
```
