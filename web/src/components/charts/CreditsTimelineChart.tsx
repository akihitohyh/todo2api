import {
  AreaChart,
  Area,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from 'recharts'
import type { DailyPoint } from '@/types'

interface Props {
  data: DailyPoint[]
}

export function CallsTimelineChart({ data }: Props) {
  if (data.length === 0) {
    return (
      <div className="flex items-center justify-center h-48 text-muted-foreground text-sm">
        暂无数据
      </div>
    )
  }

  return (
    <ResponsiveContainer width="100%" height={240}>
      <AreaChart data={data} margin={{ top: 8, right: 16, left: 0, bottom: 0 }}>
        <defs>
          <linearGradient id="callsGrad" x1="0" y1="0" x2="0" y2="1">
            <stop offset="5%" stopColor="#6366f1" stopOpacity={0.3} />
            <stop offset="95%" stopColor="#6366f1" stopOpacity={0} />
          </linearGradient>
        </defs>
        <CartesianGrid strokeDasharray="3 3" className="stroke-border" />
        <XAxis
          dataKey="date"
          tick={{ fontSize: 11 }}
          tickLine={false}
          interval="preserveStartEnd"
          className="fill-muted-foreground"
        />
        <YAxis
          tick={{ fontSize: 11 }}
          tickLine={false}
          axisLine={false}
          allowDecimals={false}
          className="fill-muted-foreground"
        />
        <Tooltip
          formatter={(v) => [`${v} 次`, '请求数']}
          contentStyle={{ fontSize: 12 }}
        />
        <Area
          type="monotone"
          dataKey="count"
          name="请求次数"
          stroke="#6366f1"
          strokeWidth={2}
          fill="url(#callsGrad)"
          dot={false}
        />
      </AreaChart>
    </ResponsiveContainer>
  )
}
