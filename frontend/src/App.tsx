import { useEffect, useState } from 'react'

const API_BASE = (import.meta as any).env?.VITE_API_BASE || 'http://localhost:8080'

type EngineerStat = {
  login: string
  merged_pr_count: number
  output_score: number
  median_cycle_time_hours: number
  median_time_to_first_review_hours: number
  avg_iteration_cost: number
  impact_score: number
  score_breakdown: {
    output_score_percentile: number
    cycle_time_percentile: number
    time_to_first_review_percentile: number
    iteration_cost_percentile: number
  }
}

type EngineerStatsResponse = {
  repo: string
  days: number
  top: EngineerStat[]
  generated_at: string
}

export default function App() {
  const [data, setData] = useState<EngineerStatsResponse | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()

    async function run() {
      try {
        setError(null)
        const res = await fetch(`${API_BASE}/api/engineer-stats?days=90`, {
          signal: controller.signal,
        })
        if (!res.ok) throw new Error(`Request failed: ${res.status}`)
        const json = (await res.json()) as EngineerStatsResponse
        setData(json)
      } catch (e) {
        if ((e as any)?.name === 'AbortError') return
        setError((e as Error).message)
      }
    }

    run()
    return () => controller.abort()
  }, [])

  return (
    <div style={{ fontFamily: 'system-ui, sans-serif', padding: 24, maxWidth: 720 }}>
      <h1 style={{ margin: 0 }}>Weave Impact Prototype</h1>
      <p style={{ marginTop: 8, color: '#444' }}>Top engineers by impact score (last 90 days)</p>

      {!data && !error && <p>Loading…</p>}
      {error && <p style={{ color: 'crimson' }}>Error: {error}</p>}

      {data && (
        <div style={{ marginTop: 16 }}>
          <div style={{ fontSize: 12, color: '#666' }}>
            Repo: {data.repo} | Generated: {new Date(data.generated_at).toLocaleString()}
          </div>

		  <div style={{ marginTop: 14, padding: 12, border: '1px solid #eee', borderRadius: 8, background: '#fafafa' }}>
			<div style={{ fontWeight: 600, marginBottom: 6 }}>How Impact Score is calculated</div>
			<div style={{ fontSize: 13, color: '#444', lineHeight: 1.4 }}>
				<div style={{ marginBottom: 8 }}>
					We compute 4 metrics per engineer over the last {data.days} days, convert each metric to a percentile across engineers, then average the percentiles. Merged PR count is shown as supporting context.
				</div>
				<ol style={{ margin: 0, paddingLeft: 18 }}>
					<li style={{ marginBottom: 4 }}>
						<strong>Output</strong>: sum of <code>log1p(additions + deletions) + 0.3 * log1p(changedFiles)</code> across merged PRs. Higher is better.
					</li>
					<li style={{ marginBottom: 4 }}>
						<strong>Cycle time</strong>: median time from PR created → merged (hours). Lower is better (we invert the percentile).
					</li>
					<li style={{ marginBottom: 4 }}>
						<strong>Time to first review</strong>: median time from PR created → first review submitted (hours). Lower is better (we invert the percentile).
					</li>
					<li>
						<strong>Iteration cost</strong>: average of <code>(post-review commits) / log1p(linesChanged)</code> over reviewed PRs. Lower is better (we invert the percentile).
					</li>
				</ol>
				<div style={{ marginTop: 10 }}>
					<strong>Final impact score</strong> = average of the 4 percentiles (equal weights).
				</div>
			</div>
		  </div>

          <ol style={{ marginTop: 12 }}>
            {data.top.map((e) => (
              <li key={e.login} style={{ marginBottom: 6 }}>
                <div>
                  <strong>{e.login}</strong>
                </div>
                <div style={{ color: '#111', fontSize: 13 }}>
                  Impact score: <strong>{e.impact_score.toFixed(3)}</strong>
                </div>
                <div style={{ color: '#777', fontSize: 12 }}>
                  {e.merged_pr_count} merged PRs (context)
                </div>
                <div style={{ color: '#555', fontSize: 13 }}>
                  Output score: {e.output_score.toFixed(1)} | Median cycle time: {e.median_cycle_time_hours.toFixed(1)}h
                </div>
                <div style={{ color: '#555', fontSize: 13 }}>
                  Median time to first review: {e.median_time_to_first_review_hours.toFixed(1)}h | Avg iteration cost: {e.avg_iteration_cost.toFixed(2)}
                </div>
                <div style={{ color: '#777', fontSize: 12 }}>
                  Percentiles — Output: {(e.score_breakdown.output_score_percentile * 100).toFixed(0)}% | Speed: {(e.score_breakdown.cycle_time_percentile * 100).toFixed(0)}% | First review: {(e.score_breakdown.time_to_first_review_percentile * 100).toFixed(0)}% | Iteration: {(e.score_breakdown.iteration_cost_percentile * 100).toFixed(0)}%
                </div>
              </li>
            ))}
          </ol>
        </div>
      )}
    </div>
  )
}
