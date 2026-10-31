import { Routes, Route, Navigate } from 'react-router-dom'
import SubmitPage from '@/components/SubmitPage'
import TrackPage from '@/components/TrackPage'
import SubmittedPage from '@/components/SubmittedPage'

export default function App() {
  return (
    <Routes>
      <Route path="/" element={<SubmitPage />} />
      <Route path="/track/:trackingId" element={<TrackPage />} />
      <Route path="/submitted/:trackingId" element={<SubmittedPage />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
