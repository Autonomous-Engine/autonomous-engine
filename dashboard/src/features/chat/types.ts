export interface PageContext {
  page: string
  workItemId?: string
  title?: string
  boardFilter?: string
}

export interface Product {
  id: string
  name: string
  description: string
}

export interface ChatSession {
  id: string
  title: string | null
  page_context: PageContext | null
  active_work_item_id: string | null
  product_id?: string | null
  product_name?: string | null
  created_at: string
  updated_at: string
  last_message?: string | null
  linked_work_items: LinkedWorkItem[]
}

export interface LinkedWorkItem {
  work_item_id: string
  relationship: 'created' | 'modified' | 'referenced'
  title: string
}

export interface Proposal {
  action: string
  target_work_item_id: string
  changes: {
    old?: string
    new?: string
    rationale?: string
  }
  status: 'pending' | 'approved' | 'rejected'
}

export interface ChatMessage {
  id: string
  role: 'user' | 'assistant' | 'system'
  content: string
  tool_calls?: { tool: string; args_preview: string }[]
  proposal?: Proposal | null
  created_at: string
}

export interface SessionProduct {
  id: string
  name: string
}

export interface LLMProvider {
  id: string
  name: string
  provider_type: string
  model: string
  is_default: boolean
}

export interface SessionProvider {
  id: string
  name: string
  provider_type: string
  model: string
}

export interface ChatContextValue {
  isOpen: boolean
  activeSessionId: string | null
  pageContext: PageContext
  sendingSessions: Set<string>
  sessionProduct: SessionProduct | null
  openChat: (opts?: { workItemId?: string; workItemTitle?: string }) => void
  closeChat: () => void
  setPageContext: (ctx: PageContext) => void
  setActiveSessionId: (id: string | null) => void
  startNewSession: () => void
  markSending: (sessionId: string) => void
  clearSending: (sessionId: string) => void
  setSessionProduct: (sessionId: string, product: SessionProduct) => Promise<void>
  loadSessionProduct: (product: SessionProduct | null) => void
  sessionProvider: SessionProvider | null
  setSessionProvider: (sessionId: string, provider: SessionProvider) => Promise<void>
  loadSessionProvider: (provider: SessionProvider | null) => void
  ensureSession: (pageContext?: PageContext, workItemId?: string) => Promise<string>
}
