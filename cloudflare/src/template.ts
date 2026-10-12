// Minimal renderer for the html/template subset used by the Game Stop pages.
// Supported: {{.Field.Sub}}, {{.}} , {{$}} , {{$ fn arg}}, {{if}}/{{else if}}/{{else}}/{{end}},
// {{range}}/{{end}}, and the helper functions date, dateptr, datetime, len, eq, result.
// Values are HTML-escaped on output; expressions are not general code.
type Node =
  | { kind: 'text'; text: string }
  | { kind: 'action'; expr: string }
  | { kind: 'if'; branches: Array<{ cond: string | null; body: Node[] }> }
  | { kind: 'range'; expr: string; body: Node[] };

const ESCAPES: Record<string, string> = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
export const escapeText = (value: string) => value.replace(/[&<>"']/g, c => ESCAPES[c]!);

function parse(source: string): Node[] {
  const tokens = source.split(/(\{\{[^}]*\}\})/);
  let index = 0;
  const parseBody = (until: Set<string>): { body: Node[]; stop: string | null } => {
    const body: Node[] = [];
    while (index < tokens.length) {
      const token = tokens[index]!;
      const match = /^\{\{(-?)\s*([\s\S]*?)\s*(-?)\}\}$/.exec(token);
      if (!match) { if (token) body.push({ kind: 'text', text: token.trimStart() }); index++; continue; }
      const expr = match[2]!;
      index++;
      const head = expr.split(/\s+/)[0]!;
      if (head === 'end' || head === 'else') {
        if (until.has(head)) { return { body, stop: expr }; }
        throw new Error(`unexpected {{${expr}}}`);
      }
      if (head === 'if') {
        const branches: Array<{ cond: string | null; body: Node[] }> = [];
        let cond: string | null = expr.slice(2).trim();
        for (;;) {
          const inner = parseBody(new Set(['else', 'end']));
          branches.push({ cond, body: inner.body });
          if (inner.stop === null || inner.stop === 'end') break;
          const rest = inner.stop.slice(4).trim();
          cond = rest.startsWith('if') ? rest.slice(2).trim() : null;
        }
        body.push({ kind: 'if', branches });
        continue;
      }
      if (head === 'range') {
        const inner = parseBody(new Set(['end']));
        body.push({ kind: 'range', expr: expr.slice(5).trim(), body: inner.body });
        continue;
      }
      if (head === 'with') throw new Error('{{with}} is not supported');
      body.push({ kind: 'action', expr });
    }
    return { body, stop: null };
  };
  return parseBody(new Set()).body;
}

const cache = new Map<string, Node[]>();
const compiled = (source: string) => { let tree = cache.get(source); if (!tree) { tree = parse(source); cache.set(source, tree); } return tree; };

type Value = unknown;

function lookup(root: Value, current: Value, path: string): Value {
  let value = current;
  let rest = path;
  if (rest.startsWith('$.')) { value = root; rest = rest.slice(2); }
  else if (rest === '$') return root;
  else if (rest.startsWith('.')) { rest = rest.slice(1); }
  if (!rest) return value;
  for (const part of rest.split('.')) {
    if (value === null || value === undefined) return '';
    if (Array.isArray(value) && /^\d+$/.test(part)) value = value[Number(part)];
    else if (typeof value === 'object') value = (value as Record<string, Value>)[part];
    else if (typeof value === 'function') value = (value as (...a: Value[]) => Value)();
    else return '';
  }
  return value;
}

const truthy = (value: Value) => {
  if (Array.isArray(value)) return value.length > 0;
  if (value === null || value === undefined || value === false || value === 0 || value === '') return false;
  return true;
};

function utc(value: Value): string {
  const parsed = typeof value === 'string' ? Date.parse(value) : value instanceof Date ? value.getTime() : NaN;
  if (!Number.isFinite(parsed)) return '时间未知';
  const date = new Date(parsed);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())} ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())} UTC`;
}

function beijing(value: Value): string {
  const parsed = typeof value === 'string' ? Date.parse(value) : NaN;
  if (!Number.isFinite(parsed)) return '时间未知';
  // Asia/Shanghai is UTC+8 with no DST: format deterministically instead of relying on ICU data.
  const date = new Date(parsed + 8 * 3600 * 1000);
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())} ${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`;
}

function callExpression(root: Value, current: Value, source: string): Value {
  const literals = source.match(/"(?:[^"\\]|\\.)*"|\S+/g) ?? [];
  const first = literals[0] ?? '';
  switch (first) {
    case 'len': return (lookup(root, current, literals[1] ?? '') as unknown[] | string | null)?.length ?? 0;
    case 'date': return utc(lookup(root, current, literals[1] ?? ''));
    case 'dateptr': { const v = lookup(root, current, literals[1] ?? ''); return v ? utc(v) : ''; }
    case 'datetime': return new Date(Date.parse(String(lookup(root, current, literals[1] ?? ''))) || 0).toISOString();
    case 'eq': return String(lookup(root, current, literals[1] ?? '')) === String(lookup(root, current, literals[2] ?? ''));
    case 'result': {
      const value = Number(lookup(root, current, literals[1] ?? ''));
      return literals[2] === '"time-ms"' ? `${(value / 1000).toFixed(2)} 秒` : String(value);
    }
    default: {
      // Method call such as {{$.BoardName .}}: the range item is the sole argument.
      const owner = first.startsWith('$.') ? root : current;
      const name = first.replace(/^\$\./, '').replace(/^\./, '');
      const fn = lookup(root, owner, name);
      if (typeof fn === 'function') return (fn as (...a: Value[]) => Value)(...(literals[1] ? [lookup(root, current, literals[1])] : []));
      return '';
    }
  }
}

function evaluate(root: Value, current: Value, expr: string): Value {
  const trimmed = expr.trim();
  if (trimmed.startsWith('not ')) return !truthy(evaluate(root, current, trimmed.slice(4)));
  if (/^["']/.test(trimmed)) return trimmed.replace(/^["']|["']$/g, '');
  if (trimmed.startsWith('$ ') || trimmed.startsWith('$.')) return callExpression(root, current, trimmed);
  if (/^(len|date|dateptr|datetime|eq|result)\s/.test(trimmed)) return callExpression(root, current, trimmed);
  return lookup(root, current, trimmed);
}

function renderNodes(nodes: Node[], root: Value, current: Value, out: string[]): void {
  for (const node of nodes) {
    if (node.kind === 'text') { out.push(node.text); continue; }
    if (node.kind === 'action') { const value = evaluate(root, current, node.expr); out.push(escapeText(value === null || value === undefined ? '' : String(value))); continue; }
    if (node.kind === 'if') {
      for (const branch of node.branches) {
        if (branch.cond === null || truthy(evaluate(root, current, branch.cond))) { renderNodes(branch.body, root, current, out); break; }
      }
      continue;
    }
    const items = evaluate(root, current, node.expr);
    if (Array.isArray(items)) for (const item of items) renderNodes(node.body, root, item, out);
    else if (items && typeof items === 'object') renderNodes(node.body, root, items, out);
  }
}

export function renderTemplate(source: string, data: Record<string, unknown>): string {
  const out: string[] = [];
  renderNodes(compiled(source), data, data, out);
  return out.join('');
}
