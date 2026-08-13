import { cn } from '@/lib/utils'

export function Spinner({ className }: { className?: string }) {
  return (
    <div className={cn('h-8 w-8 animate-spin rounded-full border-2 border-primary border-t-transparent', className)} />
  )
}
