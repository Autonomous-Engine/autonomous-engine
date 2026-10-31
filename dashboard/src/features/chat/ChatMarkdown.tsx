import { useMemo, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import { useChat } from './ChatProvider'

marked.setOptions({ breaks: true, gfm: true })

const FILE_REF = /^([a-zA-Z0-9_\-./\\]+\.[a-zA-Z0-9]{1,10})(?::(\d+)(?:-(\d+))?)?$/

const renderer = new marked.Renderer()
const originalCode = renderer.codespan
renderer.codespan = function ({ text }: { text: string }) {
  const m = FILE_REF.exec(text)
  if (m) {
    const file = m[1]
    const line = m[2] || ''
    const escaped = text.replace(/"/g, '&quot;')
    return `<code class="code-ref" data-file="${file}" data-line="${line}" title="Open in code browser">${escaped}</code>`
  }
  // @ts-expect-error marked renderer typing
  return originalCode.call(this, { text })
}

interface Props {
  children: string
}

export default function ChatMarkdown({ children }: Props) {
  const navigate = useNavigate()
  const { sessionProduct, openChat } = useChat()

  const html = useMemo(() => {
    const raw = marked.parse(children, { async: false, renderer }) as string
    return DOMPurify.sanitize(raw, { ADD_ATTR: ['data-file', 'data-line'] })
  }, [children])

  const handleClick = useCallback((e: React.MouseEvent<HTMLDivElement>) => {
    const target = e.target as HTMLElement
    const codeRef = target.closest('.code-ref') as HTMLElement | null
    if (!codeRef) return

    e.preventDefault()
    const file = codeRef.dataset.file
    const line = codeRef.dataset.line
    if (!file) return

    const params = new URLSearchParams()
    if (sessionProduct) params.set('product', sessionProduct.id)
    params.set('file', file)
    if (line) params.set('line', line)
    openChat()
    navigate(`/code?${params.toString()}`)
  }, [navigate, sessionProduct, openChat])

  return (
    <div
      className="prose-ae"
      dangerouslySetInnerHTML={{ __html: html }}
      onClick={handleClick}
    />
  )
}
