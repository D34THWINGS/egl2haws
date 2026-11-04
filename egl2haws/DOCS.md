# EGL water (egl2haws)

Imports your daily water consumption from the **Eau du Grand Lyon** customer
area (agence.eaudugrandlyon.com, Téléo remote-read meter) into Home
Assistant's long-term statistics, for the Energy dashboard's **Water** section.

## Setup

1. Fill in the **Email** and **Password** of your customer area.
2. Optionally set **Price per m³ (TTC)**: the all-inclusive price from your
   bill (water, sewerage, fees, VAT), e.g. `3.56`. When it changes, keep the
   history right with dated prices: `3.56;2027-01-01=3.80`.
3. Start the add-on. The first run imports up to 2 years of history (a few
   seconds to a minute); check the **Log** tab for `sync finished`.
4. Go to **Settings → Dashboards → Energy → Water consumption → Add water
   source**, pick **EGL water**, and for the cost choose *Use an entity
   tracking the total costs* → **EGL water cost**.

## How it works

- Updates run at the configured times (default 08:00 and 16:00). EGL publishes
  readings with a delay of 1 to 3 days, sometimes in batches.
- Each update re-reads the last 90 days and rewrites them, so late or
  corrected readings are picked up.
- Each day's consumption is stored at midnight of that day: the Energy
  dashboard shows it as one bar per day.
- Costs are recomputed for the whole history on each update, so changing the
  price (or adding a dated one) applies to past days too.
- Nothing is stored by the add-on itself: Home Assistant's statistics are the
  only state. Uninstalling it leaves your history in place.

## Troubleshooting

Every update logs one line with `"msg":"sync finished"`, `"outcome"` and,
on failure, `"error_kind"`:

| `error_kind` | Meaning |
|---|---|
| `auth` | Email/password refused by EGL (or token refused by Home Assistant) |
| `egl_unavailable` | EGL returned a server error: it's usually temporary |
| `network` | EGL couldn't be reached |
| `egl_api_changed` | EGL answered something unexpected: the site probably changed; please open an issue |
| `home_assistant` | Home Assistant refused the statistics |

Failed updates are retried after 15 minutes and 1 hour (except `auth`).

To report an API change, the `egl2haws dump` command (Docker version) records
the responses with personal data removed; see the project README.
