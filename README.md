# Engineering Impact Dashboard (Weave Impact)

A prototype dashboard that ranks engineers by impact using GitHub merged PR data from `PostHog/posthog`. The backend aggregates PR-level metrics, converts them to percentiles across engineers, and computes a composite impact score.

## Live

- Frontend (GitHub Pages): `https://<your-gh-username>.github.io/weave-impact/`
- Backend (Render): `https://weave-impact.onrender.com`

## Impact score (high level)

We compute 4 metrics per engineer over the selected timeframe, convert each metric to a percentile across engineers (inverting where lower is better), then average the percentiles:

- Output: `log1p(additions+deletions) + 0.3*log1p(changedFiles)` summed across merged PRs (higher is better)
- Cycle time: median hours from PR created → merged (lower is better)
- Time to first review: median hours from PR created → first review submitted (lower is better)
- Iteration cost (approx): average of `(commits.totalCount - 1) / log1p(linesChanged)` for reviewed PRs (lower is better)

Merged PR count is displayed as supporting context (not directly used in the composite score).

## API

- `GET /api/pr-count?days=90`
- `GET /api/engineer-stats?days=90&max_prs=2000`
- `GET /api/engineer-stats?days=90&max_prs=2000&force=true` (bypass cache and recompute)

## Caching

- In-memory cache TTL: **30 minutes** (stale-while-revalidate)
- Persisted snapshot: written to `./cache/engineer-stats_days=..._maxprs=....json`
- If GitHub GraphQL fails, the server will serve the most recent persisted snapshot (if present) and queue a background refresh.

A GitHub Actions workflow pings the backend every 30 minutes to keep it warm and refresh the cache:

- `.github/workflows/refresh-backend-cache.yml`
  - Requires repo Actions variable: `BACKEND_URL` (example: `https://weave-impact.onrender.com`)

## Local development

### Backend (Go)

Requirements:

- Go installed
- GitHub token with repo read access

Environment:

- `GITHUB_TOKEN=<your token>`

Run:

```bash
go run ./backend
```

Server defaults to `http://localhost:8080`.

### Frontend (React + Vite)

From `./frontend`:

```bash
npm install
npm run dev
```

Environment:

- `VITE_API_BASE=http://localhost:8080` (optional)

## Deployment notes

- Frontend is configured for GitHub Pages base path in `frontend/vite.config.ts`.
- Backend is intended to run on Render and uses `PORT` provided by the platform.
