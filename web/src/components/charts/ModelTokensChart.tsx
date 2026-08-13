import { useState } from 'react'
import {
  LineChart,
  Line,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from 'recharts'
import type { ModelStatPoint } from '@/types'

const MODEL_COLORS = [
  '#6366f1', '#10b981', '#f59e0b', '#ef4444',
  '#8b5cf6', '#06b6d4', '#f97316', '#ec4899',
  '#14b8a6', '#84cc16',
]

interface Props {
  models: string[]
  daily: Record<string, ModelStatPoint[]>
  days: number
}

function shortModel(m: string) {
  return m.includes('/') ? m.split('/').pop()! : m
}

function fmtTokens(v: number) {
  if (v >= 1_000_000) return `${(v / 1_000_000).toFixed(1)}M`
  if (v >= 1_000) return `${(v / 1_000).toFixed(1)}K`
  return String(v)
}

export function ModelTokensChart({ models, daily, days }: Props) {
  const [hidden, setHidden] = useState<Set<string>>(new Set())

  if (models.length === 0) {
    return (
      <div className="flex items-center justify-center h-48 text-muted-foreground text-sm">
        暂无 Token 用量数据
      </div>
    )
  }

  const firstModel = models[0]
  const dates = (daily[firstModel] ?? []).map(p => p.date)

  const chartData = dates.map((date, i) => {
    const row: Record<string, string | number> = { date }
    for (const model of models) {
      const pts = daily[model] ?? []
      const p = pts[i]
      row[model] = (p?.input_tokens ?? 0) + (p?.output_tokens ?? 0)
    }
    return row
  })

  function toggleModel(model: string) {
    setHidden(prev => {
      const next = new Set(prev)
      if (next.has(model)) next.delete(model)
      else next.add(model)
      return next
    })
  }

  return (
    <div className="space-y-2">
      <ResponsiveContainer width="100%" height={220}>
        <LineChart data={chartData} margin={{ top: 4, right: 16, left: 0, bottom: 0 }}>
          <CartesianGrid strokeDasharray="3 3" className="stroke-border" />
          <XAxis
            dataKey="date"
            tick={{ fontSize: 11 }}
            tickLine={false}
            interval={days <= 7 ? 0 : 'preserveStartEnd'}
            className="fill-muted-foreground"
          />
          <YAxis
            tick={{ fontSize: 11 }}
            tickLine={false}
            axisLine={false}
            allowDecimals={false}
            tickFormatter={fmtTokens}
            className="fill-muted-foreground"
          />
          <Tooltip
            formatter={(v, name) => [`${fmtTokens(Number(v))} tokens`, shortModel(String(name))]}
            contentStyle={{ fontSize: 12 }}
          />
          {models.map((model, i) => (
            <Line
              key={model}
              type="monotone"
              dataKey={model}
              name={model}
              stroke={MODEL_COLORS[i % MODEL_COLORS.length]}
              strokeWidth={2}
              dot={false}
              hide={hidden.has(model)}
            />
          ))}
        </LineChart>
      </ResponsiveContainer>
      <div className="flex flex-wrap gap-x-4 gap-y-1 px-1">
        {models.map((model, i) => (
          <button
            key={model}
            type="button"
            onClick={() => toggleModel(model)}
            className="flex items-center gap-1.5 text-xs transition-opacity"
            style={{ opacity: hidden.has(model) ? 0.35 : 1 }}
          >
            <span
              className="inline-block h-2.5 w-2.5 rounded-full flex-shrink-0"
              style={{ background: MODEL_COLORS[i % MODEL_COLORS.length] }}
            />
            {shortModel(model)}
          </button>
        ))}
      </div>
    </div>
  )
}
