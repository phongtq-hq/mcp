// Bong bóng trợ lý ở góc màn hình. Trợ lý là một Agent thường của organization (khoá
// `console_assistant` trong settings): mỗi câu hỏi là một execution `single_agent` tạo bằng
// quyền của chính người dùng, nên quyền, budget và audit vẫn nằm ở API. Organization chưa
// cấu hình trợ lý thì không có bong bóng.
import { useQuery, useQueryClient, type Query } from '@tanstack/react-query';
import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from 'react';
import { Link, useLocation } from 'react-router';
import { api, get, type Schemas } from '@/api/client';
import { ErrorBox } from '@/components/ui';
import { t, tOr } from '@/i18n';
import { assistantTarget, buildMessage, MAX_QUESTION, type Turn } from '@/lib/assistant';
import { messageOf, TERMINAL } from '@/lib/execution';

type Execution = Schemas['Execution'];

interface Asked { id: number; question: string; executionId: string }

const POLL_MS = 1500;
// Lần chạy chưa kết thúc sau ngần này thì thôi hỏi lại; người dùng mở lần chạy để xem.
const GIVE_UP_MS = 3 * 60 * 1000;
const MAX_TURNS = 20;

const executionKey = (id: string) => ['execution', id, 'assistant'] as const;
const readExecution = (id: string) => () => get<Execution>('/executions/' + id);

function stalled(x: Execution, readAt: number): boolean {
  const started = Date.parse(x.created_at ?? '');
  return Number.isFinite(started) && readAt - started > GIVE_UP_MS;
}

// settled: không còn gì để chờ ở lần chạy này (đã kết thúc, đọc lỗi, hoặc chờ quá lâu).
function settled(data: Execution | undefined, failed: boolean, readAt: number): boolean {
  if (failed) return true;
  return !!data && (TERMINAL.has(data.status) || stalled(data, readAt));
}

function pollEvery(q: Query<Execution>): number | false {
  return settled(q.state.data, q.state.status === 'error', q.state.dataUpdatedAt) ? false : POLL_MS;
}

function Answer({ executionId }: { executionId: string }) {
  const query = useQuery({ queryKey: executionKey(executionId), queryFn: readExecution(executionId), refetchInterval: pollEvery });
  if (query.error) return <ErrorBox error={query.error} />;
  const x = query.data;
  if (!x || !settled(x, false, query.dataUpdatedAt)) return <div className="assistant-msg bot muted" role="status">{t('assistantThinking')}</div>;
  const run = <Link className="small" to={'/executions/' + executionId}>{t('assistantViewRun')}</Link>;
  if (x.status === 'completed') {
    // Trả lời của model là dữ liệu không tin cậy: chỉ vào trang dạng văn bản.
    return <div className="assistant-msg bot"><div className="answer">{messageOf(x.output) || t('assistantNoAnswer')}</div>{run}</div>;
  }
  const text = TERMINAL.has(x.status) ? t('assistantNoAnswer') + ' (' + tOr('st_' + x.status, x.status) + ')' : t('assistantStalled');
  return <div className="assistant-msg bot"><div>{text}</div>{run}</div>;
}

function Panel({ projectId, agentId, open, onClose }: { projectId: string; agentId: string; open: boolean; onClose: () => void }) {
  const location = useLocation();
  const client = useQueryClient();
  const [asked, setAsked] = useState<Asked[]>([]);
  const [text, setText] = useState('');
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const end = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);

  useEffect(() => { if (open) input.current?.focus(); }, [open]);

  // Lượt mới nhất còn đang chờ trả lời thì chưa gửi câu kế tiếp: các lượt cũ được gửi lại
  // làm ngữ cảnh, và mỗi câu hỏi là một lần chạy tốn chi phí.
  const last = asked.at(-1);
  const latest = useQuery({
    queryKey: executionKey(last?.executionId ?? ''),
    queryFn: readExecution(last?.executionId ?? ''),
    enabled: !!last,
    refetchInterval: pollEvery,
  });
  const waiting = !!last && !settled(latest.data, !!latest.error, latest.dataUpdatedAt);

  useEffect(() => { end.current?.scrollIntoView?.({ block: 'end' }); }, [asked.length, waiting]);

  const submit = async (e?: FormEvent) => {
    e?.preventDefault();
    const question = text.trim();
    if (!question || sending || waiting) return;
    const history: Turn[] = [];
    for (const turn of asked) {
      const done = client.getQueryData<Execution>(executionKey(turn.executionId));
      const answer = done?.status === 'completed' ? messageOf(done.output) : '';
      if (answer) history.push({ question: turn.question, answer });
    }
    setSending(true);
    setError(null);
    try {
      const x = await api<Execution>('POST', `/projects/${projectId}/executions`,
        { mode: 'single_agent', agent_id: agentId, input: { message: buildMessage(location.pathname, history, question) } });
      setAsked(list => [...list, { id: (list.at(-1)?.id ?? 0) + 1, question, executionId: x.id }].slice(-MAX_TURNS));
      setText('');
    } catch (err) {
      setError(err);
    } finally {
      setSending(false);
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Escape') onClose();
    else if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); void submit(); }
  };

  return (
    <section className="assistant-panel" role="dialog" aria-label={t('assistantTitle')}>
      <header className="assistant-head">
        <strong>{t('assistantTitle')}</strong>
        <span className="spacer" />
        {asked.length > 0 ? <button type="button" className="ghost" onClick={() => { setAsked([]); setError(null); }}>{t('assistantNew')}</button> : null}
        <button type="button" className="ghost" onClick={onClose}>{t('close')}</button>
      </header>
      <div className="assistant-log" aria-live="polite">
        <p className="small muted">{t('assistantLead')}</p>
        {asked.map(turn => (
          <div key={turn.id} className="assistant-turn">
            <div className="assistant-msg user"><span className="small muted">{t('assistantYou')}</span><div className="answer">{turn.question}</div></div>
            <Answer executionId={turn.executionId} />
          </div>
        ))}
        {error ? <ErrorBox error={error} /> : null}
        <div ref={end} />
      </div>
      <form className="assistant-form" onSubmit={e => { void submit(e); }}>
        <label>
          <span className="small muted">{t('assistantQuestion')}</span>
          <textarea ref={input} required rows={2} maxLength={MAX_QUESTION} placeholder={t('assistantPlaceholder')}
            value={text} onChange={e => setText(e.target.value)} onKeyDown={onKeyDown} />
        </label>
        <button className="primary" type="submit" disabled={sending || waiting}>{sending ? t('running') : t('assistantSend')}</button>
      </form>
    </section>
  );
}

export default function AssistantBubble() {
  const [open, setOpen] = useState(false);
  // Panel chỉ được dựng từ lần mở đầu tiên, rồi được giữ lại khi thu gọn: đóng bong bóng
  // không làm mất cuộc trò chuyện.
  const [used, setUsed] = useState(false);
  // Đọc được organization và có cấu hình trợ lý thì mới hiện; lỗi đọc (thiếu quyền, mất
  // mạng) nghĩa là không có bong bóng, không phải một thông báo lỗi trên mọi màn hình.
  const org = useQuery({ queryKey: ['organization', 'assistant'], queryFn: () => get<Schemas['Organization']>('/organization') });
  const target = org.data ? assistantTarget(org.data.settings) : null;
  if (!target) return null;
  return (
    <div className="assistant">
      {used ? <div hidden={!open}><Panel projectId={target.projectId} agentId={target.agentId} open={open} onClose={() => setOpen(false)} /></div> : null}
      <button type="button" className="assistant-toggle primary" aria-expanded={open} onClick={() => { setUsed(true); setOpen(v => !v); }}>
        {t('assistantTitle')}
      </button>
    </div>
  );
}
