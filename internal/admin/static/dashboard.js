const csrf = document.querySelector('meta[name="csrf-token"]').content;
const number = new Intl.NumberFormat();

function text(id, value) {
  document.getElementById(id).textContent = number.format(value);
}

function cell(value, className) {
  const td = document.createElement('td');
  td.textContent = value;
  if (className) td.className = className;
  return td;
}

function ago(value) {
  if (!value) return 'never';
  const seconds = Math.max(0, Math.floor((Date.now() - new Date(value)) / 1000));
  if (seconds < 60) return 'just now';
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86400)}d ago`;
}

function action(path, label) {
  const form = document.createElement('form');
  form.className = 'inline';
  form.method = 'post';
  form.action = path;

  const token = document.createElement('input');
  token.type = 'hidden';
  token.name = 'csrf_token';
  token.value = csrf;

  const button = document.createElement('button');
  button.className = 'secondary';
  button.textContent = label;
  form.append(token, button);
  return form;
}

function sparkline(points) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('width', '70');
  svg.setAttribute('height', '18');
  svg.setAttribute('viewBox', '0 0 70 18');

  const values = points.map(point => point.requests);
  const max = Math.max(1, ...values);
  const path = document.createElementNS(svg.namespaceURI, 'path');
  path.setAttribute('d', values.map((value, i) => {
    const x = 2 + i * 11;
    const y = 16 - value / max * 14;
    return `${i ? 'L' : 'M'}${x},${y}`;
  }).join(' '));
  path.setAttribute('fill', 'none');
  path.setAttribute('stroke', '#e6edf3');
  path.setAttribute('stroke-width', '1.5');
  svg.append(path);
  return svg;
}

function empty(tbody, columns, message) {
  const tr = document.createElement('tr');
  const td = cell(message, 'muted');
  td.colSpan = columns;
  tr.append(td);
  tbody.replaceChildren(tr);
}

function renderStats(stats) {
  text('total-requests', stats.total_requests);
  text('today-requests', stats.today_requests);
  text('total-tokens', stats.total_tokens);
  text('input-tokens', stats.input_tokens);
  text('cached-tokens', stats.cached_tokens);
  text('cache-write-tokens', stats.cache_write_tokens);
  text('output-tokens', stats.output_tokens);
  text('reasoning-tokens', stats.reasoning_tokens);
  text('web-searches', stats.web_search_requests);
  text('image-tokens', stats.image_gen_total_tokens);
  text('active-clients', stats.active_api_keys);
  text('healthy-tokens', stats.healthy_tokens);
}

function renderKeys(keys, usage) {
  const tbody = document.getElementById('api-keys');
  if (!keys.length) return empty(tbody, 11, 'No API keys yet.');
  tbody.replaceChildren(...keys.map(key => {
    const tr = document.createElement('tr');
    tr.append(
      cell(key.username), cell(key.app_name), cell(key.machine_name),
      cell(`${key.key_prefix}…`), cell(key.rate_limit_rps.toFixed(1)),
      cell(number.format(key.total_requests)), cell(number.format(key.total_tokens)),
      cell(ago(key.last_used_at))
    );
    const chart = document.createElement('td');
    chart.append(sparkline(usage[key.id] || []));
    tr.append(chart, cell(key.enabled ? 'active' : 'revoked', key.enabled ? 'healthy' : 'muted'));
    const actions = document.createElement('td');
    if (key.enabled) actions.append(action(`/admin/keys/${key.id}/revoke`, 'Revoke'));
    tr.append(actions);
    return tr;
  }));
}

function renderTokens(tokens) {
  const tbody = document.getElementById('token-pool');
  if (!tokens.length) return empty(tbody, 7, 'No donated tokens yet.');
  tbody.replaceChildren(...tokens.map(token => {
    const limited = token.limited_until && new Date(token.limited_until) > new Date();
    const active = token.enabled && token.healthy && !limited;
    const status = !token.enabled ? 'removed'
      : !token.healthy ? 'unhealthy'
      : limited ? `limited until ${new Date(token.limited_until).toLocaleString()}`
      : 'healthy';
    const tr = document.createElement('tr');
    tr.append(
      cell(token.label), cell(token.account_id),
      cell(new Date(token.access_token_expires_at).toLocaleString()),
      cell(ago(token.last_refreshed_at)), cell(token.consecutive_failures),
      cell(status, active ? 'healthy' : token.enabled ? 'unhealthy' : 'muted')
    );
    const actions = document.createElement('td');
    if (token.enabled) actions.append(action(`/admin/tokens/${token.id}/remove`, 'Remove'));
    tr.append(actions);
    return tr;
  }));
}

function renderRequests(requests) {
  const tbody = document.getElementById('recent-requests');
  if (!requests.length) return empty(tbody, 11, 'No requests yet.');
  tbody.replaceChildren(...requests.map(request => {
    const tr = document.createElement('tr');
    tr.append(
      cell(ago(request.created_at)), cell(request.client), cell(request.token), cell(request.model),
      cell(request.status_code), cell(number.format(request.input_tokens)),
      cell(number.format(request.cached_tokens)),
      cell(number.format(request.output_tokens)), cell(number.format(request.total_tokens)),
      cell(`${request.duration_ms}ms`), cell(request.streamed ? 'stream' : 'buffered')
    );
    return tr;
  }));
}

async function refresh() {
  const status = document.getElementById('live-status');
  try {
    const response = await fetch('/admin/data', {cache: 'no-store'});
    if (!response.ok) throw new Error(response.statusText);
    const data = await response.json();
    renderStats(data.stats);
    renderKeys(data.api_keys, data.key_usage);
    renderTokens(data.tokens);
    renderRequests(data.requests);
    status.textContent = 'live';
    status.className = 'healthy';
  } catch {
    status.textContent = 'reconnecting';
    status.className = 'unhealthy';
  }
  setTimeout(refresh, document.hidden ? 10000 : 2000);
}

refresh();
