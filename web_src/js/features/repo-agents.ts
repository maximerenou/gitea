import {registerGlobalInitFunc} from '../modules/observer.ts';
import {GET} from '../modules/fetch.ts';
import {html, htmlRaw} from '../utils/html.ts';
import {createElementFromHTML} from '../utils/dom.ts';
import {initRepoIssueContentHistory} from './repo-issue-content.ts';

type AgentTodo = {text: string, completed: boolean};

export type AgentEvent = {
  id: number,
  runId: number,
  key: string,
  kind: string,
  title: string,
  body: string,
  status: string,
  todos?: AgentTodo[],
  html?: string, // Markdown rendered and sanitized by the server
  diff?: boolean,
};

type AgentEventsResponse = {
  lastId: number,
  events: AgentEvent[],
  active: boolean,
  complete: boolean,
  runs: Array<{id: number, status: string}>,
};

function renderDiff(diff: string): string {
  const lines = diff.split('\n').map((line) => {
    let cls = '';
    if (line.startsWith('+') && !line.startsWith('+++')) cls = 'added';
    else if (line.startsWith('-') && !line.startsWith('---')) cls = 'removed';
    else if (line.startsWith('@@')) cls = 'hunk';
    return html`<span class="${cls}">${line}</span>`;
  });
  return html`<pre class="agent-event-body agent-event-diff">${htmlRaw(lines.join('\n'))}</pre>`;
}

export function renderAgentEvent(event: AgentEvent, label: string): string {
  const status = event.status ? html`<span class="agent-event-status">${event.status}</span>` : '';
  const title = event.title ? html`<code class="agent-event-title">${event.title}</code>` : '';
  const header = html`<div class="agent-event-header"><strong>${label}</strong>${htmlRaw(title)}${htmlRaw(status)}</div>`;
  let body = '';
  if (event.todos?.length) {
    body = html`<ul class="agent-event-todos">${htmlRaw(event.todos.map((todo) => html`<li class="${todo.completed ? 'done' : ''}">${todo.completed ? '☑' : '☐'} ${todo.text}</li>`).join(''))}</ul>`;
  } else if (event.html) {
    body = html`<div class="markup agent-event-markup">${htmlRaw(event.html)}</div>`;
  } else if (event.diff) {
    body = renderDiff(event.body);
  } else if (event.body) {
    body = html`<pre class="agent-event-body">${event.body}</pre>`;
  }
  if (event.kind === 'reasoning') {
    return html`<details class="agent-event agent-event-reasoning" data-key="${event.key}"><summary>${htmlRaw(header)}</summary>${htmlRaw(body)}</details>`;
  }
  return html`<div class="agent-event agent-event-${event.kind}" data-key="${event.key}">${htmlRaw(header)}${htmlRaw(body)}</div>`;
}

const pollInterval = 2000;
// with the live connection, polling only covers changes the connection could miss
const livePollInterval = 30000;

function initAgentSession(el: HTMLElement) {
  const url = el.getAttribute('data-events-url')!;
  const liveUrl = el.getAttribute('data-live-url')!;
  const label = (kind: string) => el.getAttribute(`data-locale-${kind}`) ?? kind;
  let lastId = 0;
  let wasActive: boolean | null = null;
  let live = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let fetching = false;
  let again = false;

  const apply = (event: AgentEvent) => {
    const container = document.querySelector<HTMLElement>(`.agent-events[data-run-id="${CSS.escape(String(event.runId))}"]`);
    if (!container) return;
    const node = createElementFromHTML(renderAgentEvent(event, label(event.kind)));
    const existing = container.querySelector(`[data-key="${CSS.escape(event.key)}"]`);
    if (existing) {
      existing.replaceWith(node);
    } else {
      container.append(node);
    }
  };

  const schedule = (delay: number) => {
    clearTimeout(timer);
    timer = setTimeout(poll, delay);
  };

  async function poll() {
    if (fetching) {
      again = true;
      return;
    }
    fetching = true;
    let resp: AgentEventsResponse;
    try {
      resp = await (await GET(`${url}?after=${lastId}`)).json();
    } catch {
      fetching = false;
      schedule(pollInterval * 2);
      return;
    }
    fetching = false;
    for (const event of resp.events) apply(event);
    lastId = resp.lastId;
    // a finished session or a new run changes the page's controls, reload once to show them
    if ((wasActive && !resp.active) || resp.runs.length !== document.querySelectorAll('.agent-events').length) {
      window.location.reload();
      return;
    }
    wasActive = resp.active;
    if (!resp.complete || again) {
      again = false;
      schedule(0);
    } else if (resp.active) {
      schedule(live ? livePollInterval : pollInterval);
    }
  }

  const connect = () => {
    const socket = new WebSocket(new URL(liveUrl, window.location.href).href.replace(/^http/, 'ws'));
    socket.addEventListener('open', () => {
      live = true;
    });
    socket.addEventListener('message', () => schedule(0));
    socket.addEventListener('close', () => {
      live = false;
      schedule(0); // catch up with polling until the connection is back
      setTimeout(connect, 10000);
    });
  };
  connect();
  schedule(0);
}

// the Agents tab reloads its list when a session starts or changes status, so its status icons stay current
function initAgentSessionList(el: HTMLElement) {
  const liveUrl = el.getAttribute('data-live-url')!;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const refresh = async () => {
    try {
      const doc = new DOMParser().parseFromString(await (await GET(window.location.href)).text(), 'text/html');
      const list = doc.querySelector('#agent-session-list');
      if (list) el.innerHTML = list.innerHTML;
    } catch {} // the next change refreshes it
  };
  const connect = (reconnect: boolean) => {
    const socket = new WebSocket(new URL(liveUrl, window.location.href).href.replace(/^http/, 'ws'));
    if (reconnect) socket.addEventListener('open', refresh); // catch up with what the broken connection missed
    socket.addEventListener('message', () => {
      clearTimeout(timer);
      timer = setTimeout(refresh, 300); // a run often changes status several times in a row
    });
    socket.addEventListener('close', () => setTimeout(() => connect(true), 10000));
  };
  connect(false);
}

// the replies of a working agent update live on its issue or pull request, without touching a comment being edited
function initAgentIssueLive(el: HTMLElement) {
  const sockets = new Map<string, WebSocket>();
  let timer: ReturnType<typeof setTimeout> | undefined;
  let refreshing = false;
  const schedule = () => {
    if (timer) return;
    timer = setTimeout(() => { // the agent streams events, its reply changes at most at each checkpoint
      timer = undefined;
      refresh();
    }, 3000);
  };
  // the active sessions, and the repository's to learn about the sessions starting after the page loaded
  const wanted = () => [...el.getAttribute('data-live-urls')!.split(' '), el.getAttribute('data-repo-live-url')!].filter(Boolean);
  const connect = (liveUrl: string) => {
    const socket = new WebSocket(new URL(liveUrl, window.location.href).href.replace(/^http/, 'ws'));
    socket.addEventListener('message', schedule);
    socket.addEventListener('close', () => {
      if (sockets.get(liveUrl) !== socket) return; // closed on purpose
      sockets.delete(liveUrl);
      setTimeout(() => {
        if (!wanted().includes(liveUrl) || sockets.has(liveUrl)) return;
        connect(liveUrl);
        schedule(); // catch up with what the broken connection missed
      }, 10000);
    });
    sockets.set(liveUrl, socket);
  };
  const sync = () => {
    const urls = wanted();
    for (const [liveUrl, socket] of sockets) {
      if (urls.includes(liveUrl)) continue;
      sockets.delete(liveUrl);
      socket.close();
    }
    for (const liveUrl of urls) {
      if (!sockets.has(liveUrl)) connect(liveUrl);
    }
  };
  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      const doc = new DOMParser().parseFromString(await (await GET(window.location.pathname)).text(), 'text/html');
      const sidebar = doc.querySelector('#agent-issue-sessions');
      if (sidebar) {
        el.innerHTML = sidebar.innerHTML;
        el.setAttribute('data-live-urls', sidebar.getAttribute('data-live-urls')!);
      }
      const form = document.querySelector('.timeline-item.comment.form');
      let changed = false;
      for (const item of doc.querySelectorAll<HTMLElement>('.timeline-item.comment[id]')) {
        const current = document.querySelector<HTMLElement>(`#${CSS.escape(item.id)}`);
        if (!current) {
          if (form) form.before(document.importNode(item, true));
          changed = true;
          continue;
        }
        if (!current.querySelector('.edit-content-zone')?.classList.contains('tw-hidden')) continue; // being edited
        for (const selector of ['.render-content', '.raw-content']) {
          const from = item.querySelector(selector);
          const to = current.querySelector(selector);
          if (from && to && to.innerHTML !== from.innerHTML) {
            to.innerHTML = from.innerHTML;
            changed = true;
          }
        }
        const version = item.querySelector('.edit-content-zone')?.getAttribute('data-content-version');
        if (version) current.querySelector('.edit-content-zone')?.setAttribute('data-content-version', version);
      }
      // the other comment behaviors are delegated or set up by the global observer, not the edit history menus
      if (changed) await initRepoIssueContentHistory();
      sync();
    } catch {} // the next change refreshes it
    refreshing = false;
  }
  sync();
}

export function initRepositoryAgents() {
  registerGlobalInitFunc('initAgentSession', initAgentSession);
  registerGlobalInitFunc('initAgentSessionList', initAgentSessionList);
  registerGlobalInitFunc('initAgentIssueLive', initAgentIssueLive);
}
