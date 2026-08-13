import { useEffect, useState } from 'react'
import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { AnimatePresence, motion } from 'framer-motion'
import { api } from '@/api/client'
import { Layout } from '@/components/Layout'
import { Spinner } from '@/components/ui/spinner'

export function ProtectedRoute() {
  const [checking, setChecking] = useState(true)
  const [authed, setAuthed] = useState(false)
  const location = useLocation()

  useEffect(() => {
    api.checkAuth()
      .then(r => setAuthed(r.authenticated))
      .catch(() => setAuthed(false))
      .finally(() => setChecking(false))
  }, [])

  if (checking) {
    return (
      <div className="flex h-screen items-center justify-center bg-background">
        <Spinner />
      </div>
    )
  }

  if (!authed) {
    return <Navigate to="/login" replace />
  }

  return (
    <Layout>
      <AnimatePresence mode="wait">
        <motion.div
          key={location.pathname}
          initial={{ opacity: 0, y: 8 }}
          animate={{ opacity: 1, y: 0 }}
          exit={{ opacity: 0, y: -8 }}
          transition={{ duration: 0.2, ease: 'easeOut' }}
          className="h-full"
        >
          <Outlet />
        </motion.div>
      </AnimatePresence>
    </Layout>
  )
}
