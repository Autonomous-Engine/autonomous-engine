import { useMemo } from 'react'
import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({ breaks: true, gfm: true })

export default function Markdown({ children, className = '' }: { children: string; className?: string }) {
  const html = useMemo(() => {
    const raw = marked.parse(children, { async: false }) as string
    return DOMPurify.sanitize(raw)
  }, [children])

  return (
    <div
      className={`prose-ae ${className}`}
      dangerouslySetInnerHTML={{ __html: html }}
    />
  )
}
