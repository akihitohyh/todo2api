import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { motion, AnimatePresence } from "framer-motion";
import {
  Plus,
  Trash2,
  Loader2,
  RefreshCw,
  RotateCcw,
  ChevronLeft,
  ChevronRight,
  CheckCircle2,
  Power,
} from "lucide-react";
import { toast } from "sonner";
import { api, ApiError } from "@/api/client";
import type { Account, ReloadProgressResponse } from "@/types";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Progress } from "@/components/ui/progress";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Pagination,
  PaginationContent,
  PaginationEllipsis,
  PaginationItem,
  PaginationLink,
} from "@/components/ui/pagination";

type StatusVariant = "success" | "error" | "warning" | "offline";

const statusConfig: Record<
  Account["status"],
  { label: string; variant: StatusVariant }
> = {
  active: { label: "正常", variant: "success" },
  exhausted: { label: "限额耗尽", variant: "warning" },
  disabled: { label: "已禁用", variant: "offline" },
  invalid: { label: "凭据无效", variant: "error" },
  initializing: { label: "初始化中", variant: "warning" },
  error: { label: "检查失败", variant: "error" },
};

function StatusBadge({ status }: { status: Account["status"] }) {
  const cfg = statusConfig[status] ?? {
    label: status,
    variant: "offline" as StatusVariant,
  };
  return (
    <Badge variant={cfg.variant as Parameters<typeof Badge>[0]["variant"]}>
      {cfg.label}
    </Badge>
  );
}

function BalanceCell({ account }: { account: Account }) {
  if (account.balance_unlimited || account.balance === -1) {
    return (
      <Badge variant="outline" className="font-mono text-xs">
        无限
      </Badge>
    );
  }
  return (
    <span className="tabular-nums">{account.balance.toLocaleString()}</span>
  );
}

function formatBalanceAt(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return d.toLocaleString("zh-CN", {
    month: "numeric",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
  });
}

function RefreshBalanceButton({
  account,
  onRefreshed,
}: {
  account: Account;
  onRefreshed: (account: Account) => void;
}) {
  const [loading, setLoading] = useState(false);

  async function handleRefresh() {
    setLoading(true);
    try {
      const res = await api.refreshAccount(account.id);
      onRefreshed(res);
      toast.success(`${account.name} 健康状态已刷新`);
    } catch {
      toast.error("健康检查失败");
    } finally {
      setLoading(false);
    }
  }

  return (
    <Button
      variant="ghost"
      size="icon"
      className="h-8 w-8 text-muted-foreground hover:text-primary hover:bg-primary/10"
      onClick={handleRefresh}
      disabled={loading}
      title="健康检查"
    >
      <RefreshCw size={14} className={loading ? "animate-spin" : ""} />
    </Button>
  );
}

function ToggleButton({
  account,
  onChanged,
}: {
  account: Account;
  onChanged: (account: Account) => void;
}) {
  const [loading, setLoading] = useState(false);
  async function toggle() {
    setLoading(true);
    try {
      const updated = await api.setAccountEnabled(account.id, !account.enabled);
      onChanged(updated);
      toast.success(account.enabled ? "API Key 已禁用" : "API Key 已启用");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "操作失败");
    } finally {
      setLoading(false);
    }
  }
  return (
    <Button
      variant="ghost"
      size="icon"
      className="h-8 w-8 text-muted-foreground hover:text-primary hover:bg-primary/10"
      onClick={toggle}
      disabled={loading}
      title={account.enabled ? "禁用" : "启用"}
    >
      <Power size={14} className={loading ? "animate-pulse" : ""} />
    </Button>
  );
}

function DeleteButton({
  account,
  onDeleted,
}: {
  account: Account;
  onDeleted: () => void;
}) {
  const [loading, setLoading] = useState(false);
  const [open, setOpen] = useState(false);

  async function handleDelete() {
    setLoading(true);
    try {
      await api.deleteAccount(account.id);
      toast.success(`API Key ${account.api_key_masked} 已删除`);
      setOpen(false);
      onDeleted();
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "删除失败");
    } finally {
      setLoading(false);
    }
  }

  return (
    <AlertDialog
      open={open}
      onOpenChange={(o) => {
        if (!loading) setOpen(o);
      }}
    >
      <Button
        variant="ghost"
        size="icon"
        className="h-8 w-8 text-muted-foreground hover:text-destructive hover:bg-destructive/10"
        onClick={() => setOpen(true)}
      >
        <Trash2 size={14} />
      </Button>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>确认删除 API Key</AlertDialogTitle>
          <AlertDialogDescription>
            将从账号池和 SQLite 数据库中永久删除{" "}
            <strong>{account.api_key_masked}</strong>。此操作不可撤销。
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={loading}>取消</AlertDialogCancel>
          <AlertDialogAction
            onClick={handleDelete}
            disabled={loading}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
          >
            {loading ? (
              <>
                <Loader2 className="animate-spin" size={14} /> 删除中…
              </>
            ) : (
              "确认删除"
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function getPaginationRange(
  current: number,
  total: number,
): (number | "...")[] {
  if (total <= 7) return Array.from({ length: total }, (_, i) => i + 1);
  const result: (number | "...")[] = [1];
  if (current > 3) result.push("...");
  for (
    let i = Math.max(2, current - 1);
    i <= Math.min(total - 1, current + 1);
    i++
  ) {
    result.push(i);
  }
  if (current < total - 2) result.push("...");
  result.push(total);
  return result;
}

const PAGE_SIZES = [20, 50, 100] as const;
type PageSize = (typeof PAGE_SIZES)[number];

export function AccountsPage() {
  const navigate = useNavigate();
  const [accounts, setAccounts] = useState<Account[] | null>(null);
  const [reloading, setReloading] = useState(false);
  const [addOpen, setAddOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  const [newKey, setNewKey] = useState("");
  const [newProjectID, setNewProjectID] = useState("");
  const [newAgentID, setNewAgentID] = useState("");

  // Reload progress modal state
  const [progressOpen, setProgressOpen] = useState(false);
  const [progress, setProgress] = useState<ReloadProgressResponse>({
    running: false,
    total: 0,
    done: 0,
    exhausted: 0,
    invalid: 0,
  });
  const [isDone, setIsDone] = useState(false);
  const [reloadScope, setReloadScope] = useState<"all" | "selected">("all");
  const [reloadStartingScope, setReloadStartingScope] = useState<
    "all" | "selected" | null
  >(null);
  const reloadEsRef = useRef<EventSource | null>(null);
  const reloadRetryRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const reloadStreamActiveRef = useRef(false);

  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [batchConfirmOpen, setBatchConfirmOpen] = useState(false);
  const [batchTargets, setBatchTargets] = useState<Account[]>([]);
  const [batchDescription, setBatchDescription] = useState("");
  const [batchLoading, setBatchLoading] = useState(false);

  const [pageSize, setPageSize] = useState<PageSize>(20);
  const [page, setPage] = useState(1);

  type StatusFilter =
    | "all"
    | "active"
    | "exhausted"
    | "invalid"
    | "error"
    | "initializing"
    | "disabled";
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");

  const filteredAccounts = (accounts ?? []).filter(
    (a) => statusFilter === "all" || a.status === statusFilter,
  );
  const totalPages = Math.max(1, Math.ceil(filteredAccounts.length / pageSize));
  const pagedAccounts = filteredAccounts.slice(
    (page - 1) * pageSize,
    page * pageSize,
  );
  const pageKeys = pagedAccounts.map((a) => String(a.id));
  const allPageSelected =
    pageKeys.length > 0 && pageKeys.every((k) => selected.has(k));
  const somePageSelected =
    pageKeys.some((k) => selected.has(k)) && !allPageSelected;

  async function load() {
    try {
      const data = await api.getAccounts();
      setAccounts(data);
    } catch (err) {
      if (err instanceof Error && err.message === "Unauthorized") {
        navigate("/login", { replace: true });
      } else {
        toast.error("加载账号列表失败");
      }
    }
  }

  function closeReloadStream() {
	reloadStreamActiveRef.current = false;
    reloadEsRef.current?.close();
    reloadEsRef.current = null;
	if (reloadRetryRef.current) clearTimeout(reloadRetryRef.current);
	reloadRetryRef.current = null;
  }

  function connectReloadStream() {
	if (!reloadStreamActiveRef.current) return;
	const es = new EventSource(api.getReloadProgressStreamURL());
	reloadEsRef.current = es;
	es.onmessage = (e) => {
	  const data = e.data as string;
	  if (!data) return;
	  if (data === "[DONE]") {
		reloadStreamActiveRef.current = false;
		es.close();
		reloadEsRef.current = null;
		setIsDone(true);
		load();
		return;
	  }
	  try {
		setProgress(JSON.parse(data) as ReloadProgressResponse);
	  } catch {
		/* ignore malformed progress events */
	  }
	};
	es.onerror = () => {
	  es.close();
	  if (reloadEsRef.current === es) reloadEsRef.current = null;
	  if (reloadStreamActiveRef.current) {
		reloadRetryRef.current = setTimeout(connectReloadStream, 1500);
	  }
	};
  }

  async function startReload(
    scope: "all" | "selected",
    accountIds?: number[],
  ) {
    setReloading(true);
    setReloadStartingScope(scope);

    try {
      await api.reloadAccounts(accountIds);
	    closeReloadStream();
      setReloadScope(scope);
      setProgress({ running: true, total: 0, done: 0, exhausted: 0, invalid: 0 });
      setIsDone(false);
      setProgressOpen(true);
	    reloadStreamActiveRef.current = true;
	    connectReloadStream();
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        toast.error("已有健康检查正在进行");
      } else {
        toast.error(err instanceof Error ? err.message : "健康检查启动失败");
      }
    } finally {
      setReloading(false);
      setReloadStartingScope(null);
    }
  }

  function handleReload() {
    void startReload("all");
  }

  function handleReloadSelected() {
    const accountIds = (accounts ?? [])
      .filter((account) => selected.has(String(account.id)))
      .map((account) => account.id);
    if (accountIds.length === 0) {
      toast.info("没有可检查的所选账号");
      return;
    }
    void startReload("selected", accountIds);
  }

  function handleProgressClose() {
    closeReloadStream();
    setProgressOpen(false);
    load();
  }

  function handleAccountChanged(updated: Account) {
    setAccounts((prev) =>
      prev ? prev.map((a) => (a.id === updated.id ? updated : a)) : prev,
    );
  }

  async function handleAddAccount(e: React.FormEvent) {
    e.preventDefault();
    if (!newKey.trim()) return;
    setAdding(true);
    try {
      const created = await api.createAccount({
        api_key: newKey.trim(),
        project_id: newProjectID.trim(),
        agent_id: newAgentID.trim(),
      });
      setAccounts((prev) => (prev ? [...prev, created] : [created]));
      setNewKey("");
      setNewProjectID("");
      setNewAgentID("");
      setAddOpen(false);
      toast.success("API Key 已添加");
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "添加失败");
    } finally {
      setAdding(false);
    }
  }

  useEffect(() => {
    load();

    // SSE connection for real-time account data updates
    let es: EventSource | null = null;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let cancelled = false;

    function connectEvents() {
      if (cancelled) return;
      es = new EventSource(api.getEventsURL());
      es.onmessage = (e) => {
        if (e.data === "accounts") load();
      };
      es.onerror = () => {
        es?.close();
        if (!cancelled) retryTimer = setTimeout(connectEvents, 3000);
      };
    }
    connectEvents();

    return () => {
	  closeReloadStream();
      cancelled = true;
      es?.close();
      if (retryTimer) clearTimeout(retryTimer);
    };
  }, [navigate]);

  useEffect(() => {
	setPage((current) => Math.min(current, totalPages));
  }, [totalPages]);

  function toggleAll() {
    setSelected((prev) => {
      const next = new Set(prev);
      if (allPageSelected) pageKeys.forEach((k) => next.delete(k));
      else pageKeys.forEach((k) => next.add(k));
      return next;
    });
  }

  function toggleOne(key: string) {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  async function handleBatchDelete() {
    setBatchLoading(true);
    try {
	  const results = await Promise.allSettled(
		batchTargets.map((account) => api.deleteAccount(account.id)),
	  );
	  const deleted = new Set(
		batchTargets
		  .filter((_, index) => results[index].status === "fulfilled")
		  .map((account) => String(account.id)),
	  );
	  const failed = results.length - deleted.size;
	  setSelected((current) => {
		const next = new Set(current);
		deleted.forEach((id) => next.delete(id));
		return next;
	  });
      setBatchConfirmOpen(false);
      await load();
	  if (failed === 0) {
		toast.success(`已删除 ${deleted.size} 个账号`);
	  } else {
		toast.error(`已删除 ${deleted.size} 个账号，${failed} 个删除失败`);
	  }
    } catch {
      toast.error("批量删除失败");
    } finally {
      setBatchLoading(false);
    }
  }

  function handleCleanup() {
    const bad = (accounts ?? []).filter(
      (a) => a.status === "invalid" || a.status === "exhausted",
    );
    if (bad.length === 0) {
      toast.info("没有需要清理的账号");
      return;
    }
    const exhausted = bad.filter((a) => a.status === "exhausted").length;
    const sessionInvalid = bad.filter((a) => a.status === "invalid").length;
    const parts: string[] = [];
    if (exhausted) parts.push(`限额耗尽 ${exhausted} 个`);
    if (sessionInvalid) parts.push(`凭据无效 ${sessionInvalid} 个`);
    setBatchDescription(
      `共发现 ${bad.length} 个异常账号（${parts.join("，")}），将从代理池中永久移除，此操作不可撤销。`,
    );
    setBatchTargets(bad);
    setBatchConfirmOpen(true);
  }

  function handleDeleteSelected() {
    const targets = (accounts ?? []).filter((a) => selected.has(String(a.id)));
    setBatchDescription(
      `将永久删除已选的 ${targets.length} 个账号，此操作不可撤销。`,
    );
    setBatchTargets(targets);
    setBatchConfirmOpen(true);
  }

  function changePageSize(size: PageSize) {
    setPageSize(size);
    setPage(1);
  }

  const progressPct =
    progress.total > 0 ? Math.round((progress.done / progress.total) * 100) : 0;

  return (
    <div className="p-4 md:p-8">
      <Dialog
        open={addOpen}
        onOpenChange={(open) => {
          if (!adding) setAddOpen(open);
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>添加 API Key</DialogTitle>
            <DialogDescription>
              密钥将加密保存到 SQLite，并立即加入账号池。
            </DialogDescription>
          </DialogHeader>
          <form onSubmit={handleAddAccount} className="space-y-4">
            <div className="space-y-2">
              <Label htmlFor="api-key">API Key</Label>
              <Input
                id="api-key"
                type="password"
                autoComplete="off"
                value={newKey}
                onChange={(e) => setNewKey(e.target.value)}
                placeholder="sk-..."
                autoFocus
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="project-id">Project ID（可选）</Label>
              <Input
                id="project-id"
                value={newProjectID}
                onChange={(e) => setNewProjectID(e.target.value)}
                placeholder="留空自动选择"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="agent-id">Agent ID（可选）</Label>
              <Input
                id="agent-id"
                value={newAgentID}
                onChange={(e) => setNewAgentID(e.target.value)}
                placeholder="留空自动选择"
              />
            </div>
            <DialogFooter>
              <Button type="submit" disabled={adding || !newKey.trim()}>
                {adding ? (
                  <>
                    <Loader2 className="animate-spin" size={14} />
                    添加中…
                  </>
                ) : (
                  "添加"
                )}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      {/* Reload progress modal */}
      <Dialog
        open={progressOpen}
        onOpenChange={(open) => {
          if (!open) handleProgressClose();
        }}
      >
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>
              {isDone
                ? "检查完成"
                : reloadScope === "selected"
                  ? "正在检查所选账号健康状态…"
                  : "正在检查账号健康状态…"}
            </DialogTitle>
          </DialogHeader>
          <div className="space-y-4 pt-2">
            <div className="space-y-1.5">
              <div className="flex justify-between text-sm text-muted-foreground">
                <span>
                  已刷新 {progress.done} / 共 {progress.total} 个账号
                </span>
                <span>{progressPct}%</span>
              </div>
              <Progress value={progressPct} />
            </div>
            <div className="grid grid-cols-4 gap-2 text-center text-sm">
              <div className="rounded-lg border p-2">
                <div className="text-lg font-semibold text-green-600">
                  {progress.done}
                </div>
                <div className="text-xs text-muted-foreground">已刷新</div>
              </div>
              <div className="rounded-lg border p-2">
                <div className="text-lg font-semibold text-muted-foreground">
                  {Math.max(0, progress.total - progress.done)}
                </div>
                <div className="text-xs text-muted-foreground">待刷新</div>
              </div>
              <div className="rounded-lg border p-2">
                <div className="text-lg font-semibold text-orange-500">
                  {progress.exhausted}
                </div>
                <div className="text-xs text-muted-foreground">限额耗尽</div>
              </div>
              <div className="rounded-lg border p-2">
                <div className="text-lg font-semibold text-red-500">
                  {progress.invalid}
                </div>
                <div className="text-xs text-muted-foreground">异常</div>
              </div>
            </div>
            {isDone && (
              <div className="flex items-center justify-center gap-2 text-sm text-green-600 font-medium pt-1">
                <CheckCircle2 size={16} />
                {reloadScope === "selected"
                  ? "所选账号健康状态已更新"
                  : "所有账号健康状态已更新"}
              </div>
            )}
            <Button
              className="w-full"
              variant={isDone ? "default" : "outline"}
              onClick={handleProgressClose}
            >
              {isDone ? "完成" : "后台继续，关闭弹窗"}
            </Button>
          </div>
        </DialogContent>
      </Dialog>

      <AlertDialog
        open={batchConfirmOpen}
        onOpenChange={(open) => {
          if (!batchLoading) setBatchConfirmOpen(open);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>确认批量删除</AlertDialogTitle>
            <AlertDialogDescription>{batchDescription}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={batchLoading}>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={handleBatchDelete}
              disabled={batchLoading}
              className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            >
              {batchLoading ? (
                <>
                  <Loader2 className="animate-spin mr-1.5" size={14} />
                  删除中…
                </>
              ) : (
                "确认删除"
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      <motion.div
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
        className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between mb-6"
      >
        <div>
          <h1 className="text-2xl font-semibold text-foreground">
            API Key 管理
          </h1>
          <p className="text-sm text-muted-foreground mt-1">
            {accounts !== null
              ? statusFilter === "all"
                ? `共 ${accounts.length} 个 API Key`
                : `${filteredAccounts.length} / ${accounts.length} 个 API Key`
              : "加载中…"}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button
            variant="outline"
            size="sm"
            onClick={handleReload}
            disabled={reloading}
            className="gap-2"
          >
            <RotateCcw
              size={14}
              className={reloadStartingScope === "all" ? "animate-spin" : ""}
            />
            健康检查
          </Button>
          <Button
            variant="outline"
            size="sm"
            className="gap-2"
            onClick={handleCleanup}
            disabled={accounts === null}
          >
            <Trash2 size={14} />
            清理异常 Key
          </Button>
          <Button size="sm" className="gap-2" onClick={() => setAddOpen(true)}>
            <Plus size={14} />
            添加 API Key
          </Button>
        </div>
      </motion.div>

      <AnimatePresence>
        {selected.size > 0 && (
          <motion.div
            initial={{ opacity: 0, y: -8 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -8 }}
            transition={{ duration: 0.2 }}
            className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between rounded-lg border border-primary/30 bg-primary/5 px-4 py-2.5 mb-4 text-sm"
          >
            <span className="font-medium text-foreground">
              已选 {selected.size} 个 API Key
            </span>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setSelected(new Set())}
              >
                取消选择
              </Button>
              <Button
                size="sm"
                variant="outline"
                className="gap-1.5"
                onClick={handleReloadSelected}
                disabled={reloading}
              >
                <RefreshCw
                  size={13}
                  className={
                    reloadStartingScope === "selected" ? "animate-spin" : ""
                  }
                />
                检查所选（{selected.size}）
              </Button>
              <Button
                size="sm"
                variant="destructive"
                className="gap-1.5"
                onClick={handleDeleteSelected}
              >
                <Trash2 size={13} />
                删除所选（{selected.size}）
              </Button>
            </div>
          </motion.div>
        )}
      </AnimatePresence>

      {/* Status filter bar */}
      <div className="flex items-center gap-1 mb-3 flex-wrap">
        {(
          [
            ["all", "全部"],
            ["active", "正常"],
            ["exhausted", "限额耗尽"],
            ["invalid", "凭据无效"],
            ["error", "检查失败"],
            ["initializing", "初始化中"],
            ["disabled", "已禁用"],
          ] as const
        ).map(([value, label]) => {
          const count =
            value === "all"
              ? (accounts ?? []).length
              : (accounts ?? []).filter((a) => a.status === value).length;
          return (
            <Button
              key={value}
              size="sm"
              variant={statusFilter === value ? "default" : "ghost"}
              className="h-7 px-3 text-xs gap-1.5"
              onClick={() => {
                setStatusFilter(value);
                setPage(1);
              }}
              disabled={accounts === null}
            >
              {label}
              <span
                className={`${statusFilter === value ? "text-primary-foreground/70" : "text-muted-foreground"}`}
              >
                ({count})
              </span>
            </Button>
          );
        })}
      </div>

      {accounts === null ? (
        <div className="rounded-lg border border-border overflow-hidden">
          {Array.from({ length: 3 }).map((_, i) => (
            <div
              key={i}
              className="flex items-center gap-4 px-4 py-3 border-b border-border last:border-0"
            >
              <Skeleton className="h-4 w-4 rounded" />
              <Skeleton className="h-4 w-40" />
              <Skeleton className="h-5 w-14 rounded-full" />
              <Skeleton className="h-4 w-20 ml-auto" />
            </div>
          ))}
        </div>
      ) : (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          transition={{ duration: 0.3 }}
          className="rounded-lg border border-border overflow-hidden overflow-x-auto"
        >
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-10 pr-0">
                  <Checkbox
                    checked={
                      somePageSelected ? "indeterminate" : allPageSelected
                    }
                    onCheckedChange={toggleAll}
                  />
                </TableHead>
                <TableHead>API Key</TableHead>
                <TableHead>状态</TableHead>
                <TableHead className="text-right">余额</TableHead>
                <TableHead className="text-right hidden md:table-cell">
                  上次刷新
                </TableHead>
                <TableHead className="w-20"></TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              <AnimatePresence mode="popLayout">
                {pagedAccounts.length === 0 ? (
                  <TableRow>
                    <TableCell
                      colSpan={6}
                      className="text-center text-muted-foreground py-12"
                    >
                      {accounts.length === 0
                        ? "暂无 API Key，点击右上角添加"
                        : filteredAccounts.length === 0
                          ? "当前筛选条件下没有账号"
                          : "当前页无数据"}
                    </TableCell>
                  </TableRow>
                ) : (
                  pagedAccounts.map((account, idx) => (
                    <motion.tr
                      key={account.id}
                      initial={{ opacity: 0, y: 8 }}
                      animate={{ opacity: 1, y: 0 }}
                      exit={{ opacity: 0, x: -20 }}
                      transition={{ duration: 0.25, delay: idx * 0.04 }}
                      className={`border-b transition-colors hover:bg-muted/50 ${selected.has(String(account.id)) ? "bg-primary/5" : ""}`}
                    >
                      <TableCell className="pr-0">
                        <Checkbox
                          checked={selected.has(String(account.id))}
                          onCheckedChange={() => toggleOne(String(account.id))}
                        />
                      </TableCell>
                      <TableCell>
                        <div>
                          <p className="font-medium text-foreground">
                            {account.name || "API Key"}
                          </p>
                          {(account.project_id || account.agent_id) && (
                            <p className="text-xs text-muted-foreground mt-0.5">
                              {account.project_id || "自动项目"} ·{" "}
                              {account.agent_id || "自动 Agent"}
                            </p>
                          )}
                          <p className="text-xs text-muted-foreground font-mono mt-0.5">
                            {account.api_key_masked}
                          </p>
                        </div>
                      </TableCell>
                      <TableCell>
                        <StatusBadge status={account.status} />
                      </TableCell>
                      <TableCell className="text-right">
                        <BalanceCell account={account} />
                      </TableCell>
                      <TableCell className="text-right text-muted-foreground text-xs hidden md:table-cell">
                        {formatBalanceAt(account.balance_at)}
                      </TableCell>
                      <TableCell>
                        <div className="flex items-center justify-end gap-1">
                          <RefreshBalanceButton
                            account={account}
                            onRefreshed={handleAccountChanged}
                          />
                          <ToggleButton
                            account={account}
                            onChanged={handleAccountChanged}
                          />
                          <DeleteButton account={account} onDeleted={load} />
                        </div>
                      </TableCell>
                    </motion.tr>
                  ))
                )}
              </AnimatePresence>
            </TableBody>
          </Table>

          {filteredAccounts.length > 0 && (
            <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3 border-t border-border bg-muted/20 text-xs text-muted-foreground">
              <div className="flex items-center gap-1.5">
                <span>每页</span>
                {PAGE_SIZES.map((size) => (
                  <Button
                    key={size}
                    size="sm"
                    variant={pageSize === size ? "default" : "ghost"}
                    className="h-6 px-2 text-xs"
                    onClick={() => changePageSize(size)}
                  >
                    {size}
                  </Button>
                ))}
                <span>条，共 {filteredAccounts.length} 条</span>
              </div>

              {totalPages > 1 && (
                <Pagination className="w-auto mx-0 justify-end">
                  <PaginationContent>
                    <PaginationItem>
                      <PaginationLink
                        aria-disabled={page === 1}
                        className={
                          page === 1
                            ? "pointer-events-none opacity-50"
                            : "cursor-pointer"
                        }
                        onClick={() => page > 1 && setPage((p) => p - 1)}
                      >
                        <ChevronLeft size={14} />
                      </PaginationLink>
                    </PaginationItem>
                    {getPaginationRange(page, totalPages).map((p, i) =>
                      p === "..." ? (
                        <PaginationItem key={`el-${i}`}>
                          <PaginationEllipsis />
                        </PaginationItem>
                      ) : (
                        <PaginationItem key={p}>
                          <PaginationLink
                            isActive={p === page}
                            className="cursor-pointer h-7 w-7 text-xs"
                            onClick={() => setPage(p as number)}
                          >
                            {p}
                          </PaginationLink>
                        </PaginationItem>
                      ),
                    )}
                    <PaginationItem>
                      <PaginationLink
                        aria-disabled={page === totalPages}
                        className={
                          page === totalPages
                            ? "pointer-events-none opacity-50"
                            : "cursor-pointer"
                        }
                        onClick={() =>
                          page < totalPages && setPage((p) => p + 1)
                        }
                      >
                        <ChevronRight size={14} />
                      </PaginationLink>
                    </PaginationItem>
                  </PaginationContent>
                </Pagination>
              )}
            </div>
          )}
        </motion.div>
      )}
    </div>
  );
}
