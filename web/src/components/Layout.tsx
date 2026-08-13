import { useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import {
  LayoutDashboard,
  Users,
  LogOut,
  Sun,
  Moon,
  Monitor,
  Menu,
} from "lucide-react";
import { useTheme } from "next-themes";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Sheet, SheetContent, SheetTrigger } from "@/components/ui/sheet";
import { api } from "@/api/client";

const navItems = [
  { to: "/dashboard", icon: LayoutDashboard, label: "概览" },
  { to: "/accounts", icon: Users, label: "账号管理" },
];

interface NavContentProps {
  onNavigate?: () => void;
}

function NavContent({ onNavigate }: NavContentProps) {
  const location = useLocation();
  const navigate = useNavigate();
  const { setTheme } = useTheme();

  async function handleLogout() {
    await api.logout().catch(() => {});
    navigate("/login");
  }

  return (
    <div className="flex flex-col h-full">
      <div className="h-16 flex items-center px-6 border-b border-border shrink-0">
        <span className="text-lg font-semibold tracking-tight text-foreground">
          todo2api
        </span>
      </div>

      <nav className="flex-1 p-3 space-y-1 overflow-y-auto">
        {navItems.map(({ to, icon: Icon, label }) => {
          const active =
            location.pathname === to || location.pathname.startsWith(to + "/");
          return (
            <Link key={to} to={to} onClick={onNavigate}>
              <Button
                variant="ghost"
                className={cn(
                  "w-full justify-start gap-3 h-10 px-3 transition-all duration-200",
                  active
                    ? "bg-primary/10 text-primary hover:bg-primary/15"
                    : "text-muted-foreground hover:text-foreground hover:bg-accent",
                )}
              >
                <Icon size={16} />
                {label}
              </Button>
            </Link>
          );
        })}
      </nav>

      <div className="p-3 space-y-1 border-t border-border shrink-0">
        <Separator className="mb-2" />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              className="w-full justify-start gap-3 h-10 px-3 text-muted-foreground hover:text-foreground"
            >
              <Sun size={16} className="dark:hidden" />
              <Moon size={16} className="hidden dark:block" />
              切换主题
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" side="right">
            <DropdownMenuItem
              onClick={() => setTheme("light")}
              className="gap-2"
            >
              <Sun size={14} /> 浅色
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => setTheme("dark")}
              className="gap-2"
            >
              <Moon size={14} /> 深色
            </DropdownMenuItem>
            <DropdownMenuItem
              onClick={() => setTheme("system")}
              className="gap-2"
            >
              <Monitor size={14} /> 跟随系统
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>

        <Button
          variant="ghost"
          className="w-full justify-start gap-3 h-10 px-3 text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-all duration-200"
          onClick={handleLogout}
        >
          <LogOut size={16} />
          退出登录
        </Button>
      </div>
    </div>
  );
}

export function Layout({ children }: { children: React.ReactNode }) {
  const [sheetOpen, setSheetOpen] = useState(false);
  const location = useLocation();

  return (
    <div className="flex h-screen bg-background">
      {/* Desktop sidebar — hidden below md */}
      <aside className="hidden md:flex w-56 flex-col border-r border-border bg-card shrink-0">
        <NavContent />
      </aside>

      <div className="flex flex-col flex-1 min-w-0">
        {/* Mobile top bar — shown below md */}
        <header className="flex md:hidden h-14 items-center border-b border-border bg-card px-4 gap-3 shrink-0">
          <Sheet open={sheetOpen} onOpenChange={setSheetOpen}>
            <SheetTrigger asChild>
              <Button variant="ghost" size="icon" className="shrink-0">
                <Menu size={20} />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" className="w-56 p-0">
              <NavContent onNavigate={() => setSheetOpen(false)} />
            </SheetContent>
          </Sheet>
          <span className="font-semibold text-foreground truncate">
            {navItems.find((n) => location.pathname.startsWith(n.to))?.label ??
              "todo2api"}
          </span>
        </header>

        {/* Main content */}
        <main className="flex-1 overflow-auto">{children}</main>
      </div>
    </div>
  );
}
