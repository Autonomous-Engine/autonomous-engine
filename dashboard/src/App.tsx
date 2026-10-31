import { useEffect } from 'react'
import { Routes, Route, Navigate, useNavigate, useLocation } from 'react-router-dom'
import Layout from '@/components/Layout'
import PipelineBoard from '@/components/PipelineBoard'
import AgentActivity from '@/components/AgentActivity'
import ApprovalQueue from '@/components/ApprovalQueue'
import CodexGraphPage from '@/components/codex/CodexGraphPage'
import MetricsDashboard from '@/components/MetricsDashboard'
import Settings from '@/components/Settings'
import ProductManagement from '@/components/admin/ProductManagement'
import CredentialManager from '@/components/admin/CredentialManager'
import ProvidersPage from '@/components/admin/ProvidersPage'
import EmbeddingProviders from '@/components/admin/EmbeddingProviders'
import ReindexPanel from '@/components/admin/ReindexPanel'
import IntegrationConfig from '@/components/admin/IntegrationConfig'
import SystemConfig from '@/components/admin/SystemConfig'
import AuditLog from '@/components/admin/AuditLog'
import AuthConfig from '@/components/admin/AuthConfig'
import UserManagement from '@/components/admin/UserManagement'
import PromptManagement from '@/components/admin/PromptManagement'
import AgentDefinitions from '@/components/admin/AgentDefinitions'
import ConfigVersions from '@/components/admin/ConfigVersions'
import Rollouts from '@/components/admin/Rollouts'
import WebhookDeadLetters from '@/components/admin/WebhookDeadLetters'
import WatchedBranchesPage from '@/components/codex/WatchedBranchesPage'
import PendingChanges from '@/components/admin/PendingChanges'
import OnboardingWizard from '@/components/OnboardingWizard'
import Login from '@/components/Login'
import ChangePassword from '@/components/ChangePassword'
import WorkItemDetailPage from '@/components/WorkItemDetail'
import CodeBrowser from '@/components/CodeBrowser'
import CommitBrowser from '@/components/CommitBrowser'
import ChatPage from '@/features/chat/ChatPage'

// ─── Auth Guard ───────────────────────────────────────────────────────────────

function AuthGuard({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate()
  const location = useLocation()

  useEffect(() => {
    // Capture token from query param (SAML callback)
    const params = new URLSearchParams(location.search)
    const urlToken = params.get('token')
    if (urlToken) {
      localStorage.setItem('auth_token', urlToken)
      // Strip query param from URL without re-render loop
      navigate(location.pathname, { replace: true })
      return
    }

    if (!localStorage.getItem('auth_token')) {
      navigate('/login', { replace: true })
    }
  }, [location, navigate])

  if (!localStorage.getItem('auth_token')) {
    return null
  }

  return <>{children}</>
}

// ─── App ──────────────────────────────────────────────────────────────────────

export default function App() {
  return (
    <Routes>
      {/* Standalone pages — no Layout wrapper */}
      <Route path="/login" element={<Login />} />
      <Route path="/change-password" element={<ChangePassword />} />

      {/* Main app — guarded by auth */}
      <Route
        element={
          <AuthGuard>
            <Layout />
          </AuthGuard>
        }
      >
        {/* Onboarding rendered inside Layout so the app is visible behind it.
            The wizard itself wraps its content in a centered modal-like card. */}
        <Route path="/onboarding" element={<OnboardingWizard />} />
        <Route path="/" element={<PipelineBoard />} />
        <Route path="/work-items/:id" element={<WorkItemDetailPage />} />
        <Route path="/agents" element={<AgentActivity />} />
        <Route path="/approvals" element={<ApprovalQueue />} />
        <Route path="/codex" element={<CodexGraphPage />} />
        <Route path="/code" element={<CodeBrowser />} />
        <Route path="/commits" element={<CommitBrowser />} />
        <Route path="/metrics" element={<MetricsDashboard />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="/chat" element={<ChatPage />} />
        <Route path="/chat/sessions/:sessionId" element={<ChatPage />} />
        <Route path="/admin/products" element={<ProductManagement />} />
        <Route path="/admin/credentials" element={<CredentialManager />} />
        <Route path="/admin/providers" element={<ProvidersPage />} />
        <Route path="/admin/embeddings" element={<EmbeddingProviders />} />
        <Route path="/admin/reindex" element={<ReindexPanel />} />
        <Route path="/admin/integrations" element={<IntegrationConfig />} />
        <Route path="/admin/system" element={<SystemConfig />} />
        <Route path="/admin/audit" element={<AuditLog />} />
        <Route path="/admin/auth" element={<AuthConfig />} />
        <Route path="/admin/users" element={<UserManagement />} />
        <Route path="/admin/prompts" element={<PromptManagement />} />
        <Route path="/admin/agents" element={<AgentDefinitions />} />
        <Route path="/admin/config-versions" element={<ConfigVersions />} />
        <Route path="/admin/rollouts" element={<Rollouts />} />
        <Route path="/admin/webhook/dead-letters" element={<WebhookDeadLetters />} />
        <Route path="/admin/codex/watched-branches" element={<WatchedBranchesPage />} />
        <Route path="/admin/pending-changes" element={<PendingChanges />} />
      </Route>

      {/* Fallback */}
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
