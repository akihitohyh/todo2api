import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { ThemeProvider } from 'next-themes'
import { Toaster } from '@/components/ui/sonner'
import App from './App'
import './globals.css'

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <ThemeProvider defaultTheme="system" storageKey="proxy-admin-theme" attribute="class">
      <BrowserRouter basename="/">
        <App />
        <Toaster richColors closeButton />
      </BrowserRouter>
    </ThemeProvider>
  </StrictMode>
)
