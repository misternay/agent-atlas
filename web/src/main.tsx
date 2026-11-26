import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App } from './App'
import { redeemSignIn } from './api/session'
import './styles.css'

const root = createRoot(document.getElementById('root')!)

void redeemSignIn().catch(() => {}).finally(() => root.render(
  <StrictMode>
    <App />
  </StrictMode>,
))
