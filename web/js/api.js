// Same-origin /api/v1 client with automatic CSRF header injection and ETag/If-Match support.

let csrfToken = '';

export function setCSRFToken(token) {
  csrfToken = token || '';
}

export async function apiRequest(path, { method = 'GET', body, headers = {}, ifMatch } = {}) {
  const reqHeaders = { ...headers };
  if (csrfToken && method !== 'GET' && method !== 'HEAD') {
    reqHeaders['X-CSRF-Token'] = csrfToken;
  }
  if (ifMatch !== undefined && ifMatch !== null) {
    reqHeaders['If-Match'] = `"v${ifMatch}"`;
  }

  let payload = body;
  if (body !== undefined && !(body instanceof FormData)) {
    reqHeaders['Content-Type'] = 'application/json';
    payload = JSON.stringify(body);
  }

  const resp = await fetch(path, {
    method,
    headers: reqHeaders,
    body: payload,
    credentials: 'same-origin',
  });

  const contentType = resp.headers.get('content-type') || '';
  let data = null;
  if (contentType.includes('application/json')) {
    data = await resp.json();
  } else {
    data = await resp.text();
  }

  if (!resp.ok) {
    const err = new Error(data?.error?.message || `HTTP ${resp.status}`);
    err.status = resp.status;
    err.code = data?.error?.code || 'http_error';
    err.payload = data;
    throw err;
  }
  return data;
}

export function formatMs(ms) {
  if (ms === null || ms === undefined) return '—';
  const clamped = Math.max(0, Math.round(Number(ms)));
  const totalSec = Math.floor(clamped / 1000);
  const remMs = clamped % 1000;
  const hrs = Math.floor(totalSec / 3600);
  const mins = Math.floor((totalSec % 3600) / 60);
  const secs = totalSec % 60;
  const pad2 = (n) => String(n).padStart(2, '0');
  const pad3 = (n) => String(n).padStart(3, '0');
  if (hrs > 0) {
    return remMs === 0
      ? `${pad2(hrs)}:${pad2(mins)}:${pad2(secs)}`
      : `${pad2(hrs)}:${pad2(mins)}:${pad2(secs)}.${pad3(remMs)}`;
  }
  return remMs === 0 ? `${pad2(mins)}:${pad2(secs)}` : `${pad2(mins)}:${pad2(secs)}.${pad3(remMs)}`;
}
