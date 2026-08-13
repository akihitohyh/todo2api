import { useEffect, useRef, useState } from 'react'
import { motion } from 'framer-motion'
import { Play, Square, Terminal } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/api/client'
import type { StartRequest } from '@/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'

type ProxyType = StartRequest['proxy_type']

export function RegistrationPage() {
  const [running, setRunning] = useState(false)
  const [logs, setLogs] = useState<string[]>([])
  const [count, setCount] = useState(1)
  const [concurrency, setConcurrency] = useState(1)
  const [proxyType, setProxyType] = useState<ProxyType>('auto')
  const [proxyURL, setProxyURL] = useState('')
  const [starting, setStopping] = useState(false)

  const logEndRef = useRef<HTMLDivElement>(null)
  const esRef = useRef<EventSource | null>(null)

  function connectSSE() {
    if (esRef.current) {
      esRef.current.close()
    }
    const es = new EventSource(api.getRegisterStreamURL())
    esRef.current = es

    es.onmessage = (e) => {
      const line = e.data
      if (!line || line === ': ping') return
      setLogs(prev => [...prev.slice(-1999), line])
    }

    es.onerror = () => {
      es.close()
      esRef.current = null
    }
  }

  function disconnectSSE() {
    esRef.current?.close()
    esRef.current = null
  }

  useEffect(() => {
    api.getRegisterStatus()
      .then(s => {
        setRunning(s.running)
        if (s.running) connectSSE()
      })
      .catch(() => {})

    return () => disconnectSSE()
  }, [])

  useEffect(() => {
    logEndRef.current?.scrollIntoView({ behavior: 'smooth' })
  }, [logs])

  async function handleStart() {
    setStopping(true)
    try {
      await api.startRegister({
        count,
        concurrency,
        proxy_type: proxyType,
        proxy_url: proxyType === 'fixed' ? proxyURL : '',
      })
      setRunning(true)
      setLogs([])
      connectSSE()
      toast.success('注册任务已启动')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '启动失败')
    } finally {
      setStopping(false)
    }
  }

  async function handleStop() {
    setStopping(true)
    try {
      await api.stopRegister()
      setRunning(false)
      disconnectSSE()
      toast.success('注册任务已停止')
    } catch (err) {
      toast.error(err instanceof Error ? err.message : '停止失败')
    } finally {
      setStopping(false)
    }
  }

  return (
    <div className="p-4 md:p-8 space-y-6">
      <motion.div
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
        className="flex items-center gap-3"
      >
        <div>
          <h1 className="text-2xl font-semibold text-foreground">注册任务</h1>
          <p className="text-sm text-muted-foreground mt-1">批量注册</p>
        </div>
        <Badge variant={running ? 'success' : 'offline'} className="ml-2 mt-0.5">
          {running ? '运行中' : '已停止'}
        </Badge>
      </motion.div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium text-muted-foreground">任务配置</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <div className="space-y-2">
              <Label htmlFor="count">注册数量</Label>
              <Input
                id="count"
                type="number"
                min={1}
                max={1000}
                value={count}
                onChange={e => setCount(Math.max(1, parseInt(e.target.value) || 1))}
                disabled={running}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="concurrency">并发数</Label>
              <Input
                id="concurrency"
                type="number"
                min={1}
                max={20}
                value={concurrency}
                onChange={e => setConcurrency(Math.max(1, parseInt(e.target.value) || 1))}
                disabled={running}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="proxy-type">代理类型</Label>
              <Select
                value={proxyType}
                onValueChange={v => setProxyType(v as ProxyType)}
                disabled={running}
              >
                <SelectTrigger id="proxy-type">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="auto">自动</SelectItem>
                  <SelectItem value="fixed">固定代理</SelectItem>
                  <SelectItem value="none">不使用代理</SelectItem>
                </SelectContent>
              </Select>
            </div>
            {proxyType === 'fixed' && (
              <div className="space-y-2">
                <Label htmlFor="proxy-url">代理地址</Label>
                <Input
                  id="proxy-url"
                  type="text"
                  placeholder="http://host:port"
                  value={proxyURL}
                  onChange={e => setProxyURL(e.target.value)}
                  disabled={running}
                />
              </div>
            )}
          </div>

          <div className="flex gap-2 mt-5">
            <Button
              onClick={handleStart}
              disabled={running || starting}
              className="gap-2"
            >
              <Play size={14} />
              启动注册
            </Button>
            <Button
              variant="destructive"
              onClick={handleStop}
              disabled={!running || starting}
              className="gap-2"
            >
              <Square size={14} />
              停止任务
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader className="flex flex-row items-center gap-2 pb-2">
          <Terminal size={15} className="text-muted-foreground" />
          <CardTitle className="text-sm font-medium text-muted-foreground">运行日志</CardTitle>
          {logs.length > 0 && (
            <Button
              variant="ghost"
              size="sm"
              className="ml-auto h-7 text-xs text-muted-foreground"
              onClick={() => setLogs([])}
            >
              清空
            </Button>
          )}
        </CardHeader>
        <CardContent className="p-0">
          <div className="rounded-b-lg bg-zinc-950 dark:bg-zinc-900 min-h-48 max-h-[60vh] overflow-y-auto font-mono text-xs leading-5">
            {logs.length === 0 ? (
              <p className="text-zinc-500 p-4">
                {running ? '等待日志输出…' : '任务未运行，启动后此处显示实时日志'}
              </p>
            ) : (
              <div className="p-4 space-y-px">
                {logs.map((line, i) => (
                  <div key={i} className="text-green-400 whitespace-pre-wrap break-all">
                    {line}
                  </div>
                ))}
                <div ref={logEndRef} />
              </div>
            )}
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
