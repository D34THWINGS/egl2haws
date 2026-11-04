# egl2haws

EGL2HAWS is a gateway that reads data history from the **Eau du Grand Lyon**
(French water provider, Téléo remote-read meters) customer area and sends it
to Home Assistant using its websocket interface, as long-term statistics. Your
daily water consumption then shows up in the Energy dashboard's **Water**
section, with an optional cost.

It's a small standalone service (a ~7 MB static Go binary), in the spirit of
[gazpar2haws](https://github.com/ssenart/gazpar2haws) for GRDF gas. It runs as:

- a **Docker image** (`ghcr.io/d34thwings/egl2haws`, amd64 + arm64), for
  Docker, Compose or Kubernetes installs of Home Assistant;
- a **Home Assistant add-on**, for Home Assistant OS / Supervised.

> EGL has no public API. egl2haws makes the same calls as the customer website,
> which may change without notice. If it breaks, please open an issue.

## Features

- Up to 2 years of history imported on the first run.
- Two updates a day; each re-reads the last 90 days, so late or corrected
  readings (EGL publishes 1 to 3 days late, sometimes in batches) are picked up.
- Optional cost statistic from your price per m³, with dated price changes;
  recomputed on each update so price changes apply retroactively.
- Stateless: Home Assistant's statistics are the only state, no volume needed.
- Ready for Home Assistant 2026.11+ statistics imports (`mean_type`, `unit_class`).
- One JSON log line per update, with a classified error, for monitoring.

## Install

### Home Assistant add-on

[![Add repository](https://my.home-assistant.io/badges/supervisor_add_addon_repository.svg)](https://my.home-assistant.io/redirect/supervisor_add_addon_repository/?repository_url=https%3A%2F%2Fgithub.com%2FD34THWINGS%2Fegl2haws)

Or **Settings → Add-ons → Add-on store → ⋮ → Repositories**, add
`https://github.com/D34THWINGS/egl2haws`, then install **EGL water
(egl2haws)**. See the add-on's [documentation](egl2haws/DOCS.md).

### Docker

Create a [long-lived access token](https://www.home-assistant.io/docs/authentication/#your-account-profile)
in Home Assistant (your profile → Security), then:

```bash
docker run -d --name egl2haws --restart unless-stopped \
  -e EGL_USERNAME='you@example.com' \
  -e EGL_PASSWORD='your password' \
  -e HA_URL='http://homeassistant.local:8123' \
  -e HA_TOKEN='your long-lived token' \
  -e PRICE_PER_M3='3.56' \
  ghcr.io/d34thwings/egl2haws:latest
```

Docker Compose:

```yaml
services:
  egl2haws:
    image: ghcr.io/d34thwings/egl2haws:latest
    restart: unless-stopped
    environment:
      EGL_USERNAME: you@example.com
      EGL_PASSWORD: your password
      HA_URL: http://homeassistant:8123
      HA_TOKEN: your long-lived token
      PRICE_PER_M3: "3.56"
```

On Kubernetes, run it as a single-replica Deployment with the credentials in a
Secret; it needs no volume and no Service.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `EGL_USERNAME` | (required) | Email of your agence.eaudugrandlyon.com account |
| `EGL_PASSWORD` | (required) | Password of that account |
| `EGL_CONTRACT` | first contract | Contract ID, only if the account has several |
| `HA_URL` | (required) | Home Assistant URL (`http://host:8123`) or its websocket URL |
| `HA_TOKEN` | (required) | Home Assistant long-lived access token |
| `NAME` | `EGL` | Statistic names and IDs: `EGL water` (`egl2haws:egl_volume`), `EGL water cost` (`egl2haws:egl_cost`) |
| `PRICE_PER_M3` | (none) | All-inclusive price in €/m³ (TTC), e.g. `3.56`. Price changes: `3.56;2027-01-01=3.80` (a price applies from its date until the next one). Empty: no cost statistic |
| `UPDATE_TIMES` | `08:00,16:00` | Local times of the daily updates |
| `TZ` | `Europe/Paris` | Time zone of the update times and of the daily statistics |
| `HISTORY_DAYS` | `730` | Days imported on the first run |
| `REFRESH_DAYS` | `90` | Days re-read and rewritten on every update |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | `json` | `json` or `text` |

In the add-on, the same settings are in the **Configuration** tab, and the
Home Assistant connection is automatic.

Commands: `egl2haws run` (default: update now, then at each update time),
`egl2haws once` (one update, exit code 1 on failure, handy as a cron job),
`egl2haws dump` (see below), `egl2haws version`.

## Energy dashboard

**Settings → Dashboards → Energy → Water consumption → Add water source**,
pick **EGL water**. For the cost, choose *Use an entity tracking the total
costs* and pick **EGL water cost**.

Each day's consumption is stored at local midnight of that day, as one value
per day: EGL doesn't provide hourly data.

## Monitoring

Each update logs exactly one line with `"msg":"sync finished"` and
`"event":"sync"`:

- `"outcome":"success"`, with `readings`, `last_reading` (latest day EGL
  published), `publication_lag_days`, `total_liters`, `total_cost_eur`;
- `"outcome":"failure"`, with `error`, `error_system` (`egl` or
  `home_assistant`), `error_step` (`login`, `roles`, `authorize`, `token`,
  `contracts`, `readings`), `error_http_status`, and `error_kind`:

| `error_kind` | Meaning | Action |
|---|---|---|
| `auth` | Credentials refused (EGL or Home Assistant) | Fix the configuration |
| `egl_unavailable` | EGL returned a 5xx | Usually temporary |
| `network` | EGL unreachable | Usually temporary |
| `egl_api_changed` | Unexpected status or payload from EGL | The site probably changed: open an issue |
| `home_assistant` | Home Assistant refused the statistics | Check the Home Assistant logs |

Failed updates are retried after 15 minutes and 1 hour (except `auth`). A
useful alert: no `outcome:success` line in 24 hours.

## Reporting an API change

`egl2haws dump` logs in and saves EGL's responses, with personal data removed
(names, addresses, IDs, tokens; consumption figures and dates are kept), into
a folder, to attach to an issue:

```bash
docker run --rm -e EGL_USERNAME -e EGL_PASSWORD -v "$PWD/egl-dump:/egl-dump" \
  --user "$(id -u)" ghcr.io/d34thwings/egl2haws:latest dump -out /egl-dump
```

Review the files before sharing them.

## Development

```bash
go test -race ./...
go run ./cmd/egl2haws once     # with the variables above set
```

`internal/fake` contains in-process fakes of the EGL website and of Home
Assistant's websocket API, used by the tests, including an end-to-end sync.

Releases: bump `version` in `egl2haws/config.yaml`, then push a matching tag
(`v0.1.0`). The release workflow tests, builds the amd64 + arm64 image and
pushes it to ghcr.io.

## Credits

The EGL login flow and API usage were worked out by
[ha-egl-water](https://github.com/aldweb/ha-egl-water) (MIT), a Home Assistant
integration doing the same job from inside Home Assistant. Use it instead if
you prefer an integration installed with HACS.

## License

[MIT](LICENSE)
