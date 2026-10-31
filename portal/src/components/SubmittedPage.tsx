import { useState } from 'react'
import { useParams, Link } from 'react-router-dom'

export default function SubmittedPage() {
  const { trackingId } = useParams<{ trackingId: string }>()
  const [copied, setCopied] = useState(false)

  function copyToClipboard() {
    if (!trackingId) return
    navigator.clipboard.writeText(trackingId).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    })
  }

  return (
    <div className="min-h-screen bg-gray-50 flex flex-col justify-center py-12 px-4 sm:px-6 lg:px-8">
      <div className="sm:mx-auto sm:w-full sm:max-w-md">
        <div className="bg-white shadow-sm border border-gray-200 rounded-2xl p-8 text-center">

          {/* Success icon */}
          <div className="flex items-center justify-center w-16 h-16 bg-green-100 rounded-full mx-auto mb-6">
            <svg className="w-8 h-8 text-green-600" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2.5} d="M5 13l4 4L19 7" />
            </svg>
          </div>

          <h1 className="text-2xl font-bold text-gray-900 mb-2">Request Submitted Successfully</h1>
          <p className="text-gray-500 text-sm mb-8">
            Your request has been received. Use the tracking ID below to check on its progress at any time.
          </p>

          {/* Tracking ID box */}
          <div className="bg-gray-50 border border-gray-200 rounded-xl p-4 mb-3">
            <p className="text-xs text-gray-500 uppercase tracking-wider font-medium mb-2">Tracking ID</p>
            <div className="flex items-center justify-between gap-3">
              <span className="text-xl font-mono font-bold text-gray-900 tracking-wide">
                {trackingId}
              </span>
              <button
                onClick={copyToClipboard}
                className="flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium text-indigo-600 bg-indigo-50
                           hover:bg-indigo-100 rounded-lg transition focus:outline-none focus:ring-2 focus:ring-indigo-500"
              >
                {copied ? (
                  <>
                    <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2.5} d="M5 13l4 4L19 7" />
                    </svg>
                    Copied
                  </>
                ) : (
                  <>
                    <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2}
                        d="M8 16H6a2 2 0 01-2-2V6a2 2 0 012-2h8a2 2 0 012 2v2m-6 12h8a2 2 0 002-2v-8a2 2 0 00-2-2h-8a2 2 0 00-2 2v8a2 2 0 002 2z" />
                    </svg>
                    Copy
                  </>
                )}
              </button>
            </div>
          </div>

          <p className="text-xs text-gray-400 mb-8">
            Save this ID — you can track your request at any time using it.
          </p>

          {/* Action buttons */}
          <div className="flex flex-col gap-3">
            <Link
              to={`/track/${trackingId}`}
              className="w-full flex items-center justify-center gap-2 px-4 py-3 bg-indigo-600 text-white text-sm font-semibold
                         rounded-lg hover:bg-indigo-700 focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:ring-offset-2 transition"
            >
              <svg className="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2}
                  d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
              </svg>
              Track Status
            </Link>
            <Link
              to="/"
              className="w-full flex items-center justify-center gap-2 px-4 py-3 bg-white border border-gray-300 text-gray-700 text-sm font-semibold
                         rounded-lg hover:bg-gray-50 focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:ring-offset-2 transition"
            >
              Submit Another Request
            </Link>
          </div>
        </div>
      </div>
    </div>
  )
}
