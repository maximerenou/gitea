import {renderAgentEvent} from './repo-agents.ts';

test('renderAgentEvent', () => {
  const command = renderAgentEvent({id: 1, runId: 2, key: 'run-2-i1', kind: 'command', title: '<b>ls</b>', body: '<img src=x>', status: 'exit 1'}, 'Command');
  expect(command).toContain('&lt;b&gt;ls&lt;/b&gt;');
  expect(command).toContain('&lt;img src=x&gt;');
  expect(command).not.toContain('<img');

  const plan = renderAgentEvent({id: 3, runId: 2, key: 'run-2-i2', kind: 'plan', title: '', body: '', status: '', todos: [{text: 'tests', completed: true}]}, 'Plan');
  expect(plan).toContain('<li class="done">☑ tests</li>');

  const diff = renderAgentEvent({id: 4, runId: 2, key: 'k', kind: 'files', title: '', body: '@@ -1 +1 @@\n-a\n+<b>', status: '', diff: true}, 'Files');
  expect(diff).toContain('<span class="added">+&lt;b&gt;</span>');
  expect(diff).toContain('<span class="removed">-a</span>');

  const reasoning = renderAgentEvent({id: 5, runId: 2, key: 'r', kind: 'reasoning', title: '', body: 'x', status: '', html: '<p>x</p>'}, 'Reasoning');
  expect(reasoning).toMatch(/^<details class="agent-event agent-event-reasoning"/);
  expect(reasoning).toContain('<div class="markup agent-event-markup"><p>x</p></div>');
});
