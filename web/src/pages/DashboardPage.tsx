import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { motion } from "framer-motion";
import { Coins, AlertTriangle, Activity, CheckCircle2 } from "lucide-react";
import { api } from "@/api/client";
import type { DashboardStats, ModelStatsResponse } from "@/types";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { Button } from "@/components/ui/button";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { ModelSpendChart } from "@/components/charts/ModelSpendChart";
import { ModelTokensChart } from "@/components/charts/ModelTokensChart";
import { ModelCallsChart } from "@/components/charts/ModelCallsChart";

const container = {
  hidden: {},
  show: {
    transition: { staggerChildren: 0.08 },
  },
};

const item = {
  hidden: { opacity: 0, y: 20 },
  show: { opacity: 1, y: 0, transition: { duration: 0.4, ease: "easeOut" } },
};

interface StatCardProps {
  icon: React.ElementType;
  label: string;
  value: string | number;
  sub?: string;
  accent?: boolean;
}

function StatCard({ icon: Icon, label, value, sub, accent }: StatCardProps) {
  return (
    <motion.div variants={item}>
      <Card className="h-full hover:shadow-md transition-shadow duration-300">
        <CardHeader className="flex flex-row items-center justify-between space-y-0 pb-2">
          <CardTitle className="text-sm font-medium text-muted-foreground">
            {label}
          </CardTitle>
          <Icon
            size={18}
            className={accent ? "text-primary" : "text-muted-foreground"}
          />
        </CardHeader>
        <CardContent>
          <div
            className={`text-3xl font-bold tabular-nums ${accent ? "text-primary" : ""}`}
          >
            {value}
          </div>
          {sub && <p className="text-xs text-muted-foreground mt-1">{sub}</p>}
        </CardContent>
      </Card>
    </motion.div>
  );
}

function SkeletonCard() {
  return (
    <Card>
      <CardHeader className="pb-2">
        <Skeleton className="h-4 w-24" />
      </CardHeader>
      <CardContent>
        <Skeleton className="h-9 w-20 mb-1" />
        <Skeleton className="h-3 w-32" />
      </CardContent>
    </Card>
  );
}

const DAYS_OPTIONS = [7, 30] as const;
type Days = (typeof DAYS_OPTIONS)[number];

export function DashboardPage() {
  const navigate = useNavigate();
  const [stats, setStats] = useState<DashboardStats | null>(null);
  const [modelStats, setModelStats] = useState<ModelStatsResponse | null>(null);
  const [error, setError] = useState(false);
  const [days, setDays] = useState<Days>(7);

  useEffect(() => {
    let alive = true;
    function load() {
      api
        .getStats()
        .then((s) => {
          if (alive) {
            setStats(s);
            setError(false);
          }
        })
        .catch((err) => {
          if (!alive) return;
          if (err instanceof Error && err.message === "Unauthorized") {
            navigate("/login", { replace: true });
          } else {
            setError(true);
          }
        });
      api
        .getModelStats(30)
        .then((m) => {
          if (alive) setModelStats(m);
        })
        .catch(() => {});
    }
    load();

    // SSE connection for real-time stats/accounts updates
    let es: EventSource | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;

    function connectEvents() {
      if (!alive) return;
      es = new EventSource(api.getEventsURL());
      es.onmessage = (e) => {
        if ((e.data === "accounts" || e.data === "stats") && alive) load();
      };
      es.onerror = () => {
        es?.close();
        if (alive) retryTimer = setTimeout(connectEvents, 3000);
      };
    }
    connectEvents();

    return () => {
      alive = false;
      es?.close();
      if (retryTimer) clearTimeout(retryTimer);
    };
  }, [navigate]);

  const unhealthy = stats ? stats.account_count - stats.active_count : 0;

  const unhealthySub = stats
    ? (() => {
        const parts = [
          stats.exhausted_count > 0 ? `耗尽 ${stats.exhausted_count}` : null,
          stats.invalid_count > 0 ? `凭据无效 ${stats.invalid_count}` : null,
          stats.error_count > 0 ? `检查失败 ${stats.error_count}` : null,
          stats.initializing_count > 0
            ? `初始化中 ${stats.initializing_count}`
            : null,
          stats.disabled_count > 0 ? `禁用 ${stats.disabled_count}` : null,
        ].filter(Boolean);
        return parts.length > 0 ? parts.join(" · ") : "全部账号正常";
      })()
    : "非活跃账号数量";

  // Slice model data to selected day range
  const modelDailySliced: Record<string, ModelStatsResponse["daily"][string]> =
    {};
  if (modelStats) {
    for (const model of modelStats.models) {
      const pts = modelStats.daily[model] ?? [];
      modelDailySliced[model] = pts.slice(-days);
    }
  }

  // Aggregate totals for subtitles
  let totalCredits = 0,
    totalCalls = 0,
    totalTokens = 0;
  for (const model of modelStats?.models ?? []) {
    for (const pt of modelDailySliced[model] ?? []) {
      totalCredits += pt.credits ?? 0;
      totalCalls += pt.calls ?? 0;
      totalTokens += (pt.input_tokens ?? 0) + (pt.output_tokens ?? 0);
    }
  }

  function fmtNum(v: number) {
    return v.toLocaleString("en-US");
  }

  function fmtTokenAmt(v: number) {
    if (v >= 1_000_000) return `${+(v / 1_000_000).toFixed(1)}M`;
    if (v >= 1_000) return `${+(v / 1_000).toFixed(1)}K`;
    return fmtNum(v);
  }

  const DaysToggle = () => (
    <div className="flex gap-1">
      {DAYS_OPTIONS.map((d) => (
        <Button
          key={d}
          size="sm"
          variant={days === d ? "default" : "ghost"}
          className="h-7 px-3 text-xs"
          onClick={() => setDays(d)}
        >
          {d}天
        </Button>
      ))}
    </div>
  );

  return (
    <div className="p-4 md:p-8 space-y-8">
      <motion.div
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
      >
        <h1 className="text-2xl font-semibold text-foreground">概览</h1>
        <p className="text-sm text-muted-foreground mt-1">
          账号池与本地网关调用统计
        </p>
      </motion.div>

      {error && (
        <Alert variant="destructive">
          <AlertDescription>数据加载失败，请检查服务状态</AlertDescription>
        </Alert>
      )}

      {stats === null && !error ? (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <SkeletonCard key={i} />
          ))}
        </div>
      ) : stats ? (
        <motion.div
          variants={container}
          initial="hidden"
          animate="show"
          className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4"
        >
          <StatCard
            icon={Coins}
            label="账号余额"
            value={stats.total_balance.toLocaleString()}
            sub={
              stats.unlimited_count > 0
                ? `含 ${stats.unlimited_count} 个无限额度账号`
                : "仅计入有限额度账号"
            }
            accent
          />
          <StatCard
            icon={CheckCircle2}
            label="活跃账号"
            value={stats.active_count}
            sub={`共 ${stats.account_count} 个账号`}
          />
          <StatCard
            icon={AlertTriangle}
            label="异常账号"
            value={unhealthy}
            sub={unhealthySub}
          />
          <StatCard
            icon={Activity}
            label="累计请求"
            value={stats.total_calls.toLocaleString()}
            sub="仅统计经过 todo2api 的请求"
          />
        </motion.div>
      ) : null}

      {/* Side-by-side: 模型花费 (left) | 模型 Token 用量 (right) */}
      <div className="grid gap-6 lg:grid-cols-2">
        <div>
          <div className="flex items-center justify-between mb-4">
            <div>
              <h2 className="text-lg font-semibold text-foreground">
                模型花费
              </h2>
              <p className="text-xs text-muted-foreground mt-0.5">
                {modelStats
                  ? `过去 ${days} 天内，总消耗 ${fmtNum(totalCredits)}`
                  : "加载中…"}
              </p>
            </div>
            <DaysToggle />
          </div>
          <Card>
            <CardContent className="pt-6">
              {modelStats ? (
                <ModelSpendChart
                  models={modelStats.models}
                  daily={modelDailySliced}
                  days={days}
                />
              ) : (
                <Skeleton className="h-48 w-full" />
              )}
            </CardContent>
          </Card>
        </div>

        <div>
          <div className="flex items-center justify-between mb-4">
            <div>
              <h2 className="text-lg font-semibold text-foreground">
                模型 Token 用量
              </h2>
              <p className="text-xs text-muted-foreground mt-0.5">
                {modelStats
                  ? `过去 ${days} 天内，总消耗 ${fmtTokenAmt(totalTokens)}`
                  : "加载中…"}
              </p>
            </div>
            <DaysToggle />
          </div>
          <Card>
            <CardContent className="pt-6">
              {modelStats ? (
                <ModelTokensChart
                  models={modelStats.models}
                  daily={modelDailySliced}
                  days={days}
                />
              ) : (
                <Skeleton className="h-48 w-full" />
              )}
            </CardContent>
          </Card>
        </div>
      </div>

      {/* Full-width: 模型调用趋势 */}
      <div>
        <div className="flex items-center justify-between mb-4">
          <div>
            <h2 className="text-lg font-semibold text-foreground">
              模型调用趋势
            </h2>
            <p className="text-xs text-muted-foreground mt-0.5">
              {modelStats
                ? `过去 ${days} 天内，总调用 ${fmtNum(totalCalls)} 次`
                : "加载中…"}
            </p>
          </div>
          <DaysToggle />
        </div>
        <Card>
          <CardContent className="pt-6">
            {modelStats ? (
              <ModelCallsChart
                models={modelStats.models}
                daily={modelDailySliced}
                days={days}
              />
            ) : (
              <Skeleton className="h-48 w-full" />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
