import { useState } from 'react'
import { sendTestDigest } from '../api/client'

export default function TestDigestButton({ csrfToken, readOnly = false }: { csrfToken: string; readOnly?: boolean }) {
  const [state, setState] = useState<'idle' | 'sending' | 'queued' | 'error' | 'readonly'>('idle')
  async function send() {
    if (readOnly) { setState('readonly'); return }
    setState('sending')
    try {
      await sendTestDigest(csrfToken)
      setState('queued')
    } catch {
      setState('error')
    }
  }
  return <section className="digest-card" aria-labelledby="digest-title">
    <div className="eyebrow">Локальная почта</div>
    <h2 id="digest-title">Тестовый дайджест</h2>
    <p>Соберём новые рекомендации и релизы из вишлиста. Письмо появится в Mailpit.</p>
    <button className="secondary-button" type="button" onClick={() => { void send() }} disabled={state === 'sending'}>{state === 'sending' ? 'Отправляем…' : 'Отправить дайджест'}</button>
    {state === 'queued' && <p role="status">Дайджест поставлен в очередь.</p>}
    {state === 'error' && <p role="alert">Не удалось поставить дайджест в очередь.</p>}
    {state === 'readonly' && <p role="status">Демо-режим только для просмотра — отправка доступна владельцу.</p>}
  </section>
}
