// Shared Vanilla JS Domain Utilities: URL Normalization, Timestamp Parsing/Formatting, Accent Folding & Supabase Client Loader

import { VUELTARIO_CONFIG, isSupabaseConfigured } from './config.js';

let cachedSupabase = null;

export async function getSupabaseClient() {
  if (!isSupabaseConfigured()) return null;
  if (cachedSupabase) return cachedSupabase;
  const { createClient } = await import('https://cdn.jsdelivr.net/npm/@supabase/supabase-js@2/+esm');
  cachedSupabase = createClient(VUELTARIO_CONFIG.supabaseUrl, VUELTARIO_CONFIG.supabaseAnonKey, {
    auth: {
      persistSession: true,
      autoRefreshToken: true,
      detectSessionInUrl: true,
    },
  });
  return cachedSupabase;
}

// --- Safe DOM Helper (Never uses innerHTML with untrusted content) ---
export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'className') node.className = v;
    else if (k.startsWith('on') && typeof v === 'function') {
      node.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (v === true) {
      node.setAttribute(k, '');
    } else {
      node.setAttribute(k, String(v));
    }
  }
  for (const child of children) {
    if (child === null || child === undefined || child === false) continue;
    if (typeof child === 'string' || typeof child === 'number') {
      node.appendChild(document.createTextNode(String(child)));
    } else {
      node.appendChild(child);
    }
  }
  return node;
}

// --- Accent Folding, Search Keys & Dash-Tag Tokens ---
export function foldAccents(str) {
  return String(str || '')
    .normalize('NFD')
    .replace(/[\u0300-\u036f]/g, '')
    .normalize('NFC');
}

export function normalizeSearchKey(str) {
  return foldAccents(str)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, ' ')
    .trim();
}

export function portableMoveToken(canonicalName, disambiguator = '') {
  const words = foldAccents(canonicalName)
    .split(/[^A-Za-z0-9]+/)
    .filter(Boolean);
  const base = words
    .map((w) => w.charAt(0).toUpperCase() + w.slice(1).toLowerCase())
    .join('');
  const dis = foldAccents(disambiguator).replace(/[^A-Za-z0-9]+/g, '');
  return base + dis;
}

// --- Timestamps (MM:SS.mmm <-> Integer Milliseconds) ---
export function formatMs(ms, compact = true) {
  if (ms === null || ms === undefined || ms === '') return '—';
  const clamped = Math.max(0, Math.round(Number(ms)));
  const totalSec = Math.floor(clamped / 1000);
  const remMs = clamped % 1000;
  const hrs = Math.floor(totalSec / 3600);
  const mins = Math.floor((totalSec % 3600) / 60);
  const secs = totalSec % 60;
  const p2 = (n) => String(n).padStart(2, '0');
  const p3 = (n) => String(n).padStart(3, '0');
  if (hrs > 0) {
    return compact && remMs === 0 ? `${p2(hrs)}:${p2(mins)}:${p2(secs)}` : `${p2(hrs)}:${p2(mins)}:${p2(secs)}.${p3(remMs)}`;
  }
  return compact && remMs === 0 ? `${p2(mins)}:${p2(secs)}` : `${p2(mins)}:${p2(secs)}.${p3(remMs)}`;
}

export function parseTimestampMs(raw) {
  const s = String(raw ?? '').trim();
  if (!s) return null;
  const parts = s.split(':');
  let seconds = NaN;
  if (parts.length === 1) {
    seconds = Number(parts[0]);
  } else if (parts.length === 2) {
    const mins = Number(parts[0]);
    const sec = Number(parts[1]);
    if (Number.isInteger(mins) && mins >= 0 && sec >= 0 && sec < 60) {
      seconds = mins * 60 + sec;
    }
  } else if (parts.length === 3) {
    const hrs = Number(parts[0]);
    const mins = Number(parts[1]);
    const sec = Number(parts[2]);
    if (Number.isInteger(hrs) && hrs >= 0 && Number.isInteger(mins) && mins >= 0 && mins < 60 && sec >= 0 && sec < 60) {
      seconds = hrs * 3600 + mins * 60 + sec;
    }
  }
  if (!Number.isFinite(seconds) || seconds < 0) {
    throw new Error(`Invalid timestamp "${raw}" (use MM:SS, MM:SS.mmm, or seconds)`);
  }
  return Math.round(seconds * 1000);
}

export function validateBounds(startMs, endMs, durationMs) {
  if (startMs !== null && startMs < 0) throw new Error('Start timestamp must be >= 0');
  if (endMs !== null && endMs < 0) throw new Error('End timestamp must be >= 0');
  if (startMs !== null && endMs !== null && endMs <= startMs) {
    throw new Error('End timestamp must be greater than start timestamp');
  }
  if (durationMs !== null && durationMs !== undefined && durationMs > 0) {
    if (startMs !== null && startMs > durationMs) throw new Error('Start timestamp exceeds video duration');
    if (endMs !== null && endMs > durationMs) throw new Error('End timestamp exceeds video duration');
  }
}

// --- URL Normalization (ING-01 & ING-02: YouTube, TikTok, Facebook, Instagram) ---
const TRACKING_PARAMS = new Set([
  'utm_source', 'utm_medium', 'utm_campaign', 'utm_term', 'utm_content', 'utm_id',
  'fbclid', 'gclid', 'igsh', 'igshid', 'si', '_t', '_r', 'is_from_webapp',
  'sender_device', 'sender_web_id', 'feature', 'pp', 'ab_channel', 'rdid',
  'share_url', 'mibext', 'mibextid', 'ref', 'refsrc', 's',
]);

export function normalizeSocialUrl(rawInput) {
  const trimmed = String(rawInput || '').trim();
  if (!trimmed) throw new Error('URL is required');
  let u;
  try {
    u = new URL(trimmed);
  } catch {
    throw new Error(`Invalid URL: ${trimmed}`);
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    throw new Error('URL must start with http:// or https://');
  }

  const host = u.hostname.toLowerCase();
  const platform = classifyPlatform(host);
  if (!platform) {
    const err = new Error(`Unsupported host "${host}". Supported: YouTube, TikTok, Facebook, Instagram.`);
    err.originalUrl = trimmed;
    throw err;
  }

  const observedOffsetMs = extractOffsetMs(u);
  const path = u.pathname.replace(/\/+$/, '');
  let platformContentId = null;
  let canonicalUrl = '';

  if (platform === 'youtube') {
    let vid = '';
    if (host === 'youtu.be') {
      vid = path.replace(/^\//, '').split('/')[0] || '';
    } else if (path === '/watch') {
      vid = u.searchParams.get('v') || '';
    } else {
      for (const pfx of ['/shorts/', '/embed/', '/live/', '/v/']) {
        if (path.startsWith(pfx)) {
          vid = path.slice(pfx.length).split('/')[0] || '';
          break;
        }
      }
    }
    if (/^[A-Za-z0-9_-]{11}$/.test(vid)) {
      platformContentId = vid;
      canonicalUrl = path.startsWith('/shorts/')
        ? `https://www.youtube.com/shorts/${vid}`
        : `https://www.youtube.com/watch?v=${vid}`;
    } else {
      canonicalUrl = cleanFallbackUrl(u, 'www.youtube.com');
    }
  } else if (platform === 'tiktok') {
    const m = path.match(/^\/(@[^/]+)\/video\/(\d+)/);
    if (m) {
      platformContentId = m[2];
      canonicalUrl = `https://www.tiktok.com/${m[1].toLowerCase()}/video/${m[2]}`;
    } else {
      canonicalUrl = cleanFallbackUrl(u, host);
    }
  } else if (platform === 'instagram') {
    const m = path.match(/^\/(?:[^/]+\/)?(p|reel|reels|tv)\/([A-Za-z0-9_-]+)/);
    if (m) {
      const kind = m[1] === 'reels' ? 'reel' : m[1];
      platformContentId = m[2];
      canonicalUrl = `https://www.instagram.com/${kind}/${m[2]}/`;
    } else {
      canonicalUrl = cleanFallbackUrl(u, 'www.instagram.com');
    }
  } else if (platform === 'facebook') {
    if (host === 'fb.watch' && path.length > 1) {
      canonicalUrl = `https://fb.watch/${path.replace(/^\//, '')}/`;
    } else if (u.searchParams.get('v')) {
      platformContentId = u.searchParams.get('v');
      canonicalUrl = `https://www.facebook.com/watch/?v=${encodeURIComponent(platformContentId)}`;
    } else {
      const m = path.match(/\/(?:videos|reel)\/(\d+)/);
      if (m) {
        platformContentId = m[1];
        canonicalUrl = path.includes('/reel/')
          ? `https://www.facebook.com/reel/${m[1]}`
          : `https://www.facebook.com/watch/?v=${m[1]}`;
      } else {
        canonicalUrl = cleanFallbackUrl(u, 'www.facebook.com');
      }
    }
  }

  return {
    originalUrl: trimmed,
    canonicalUrl,
    platform,
    platformContentId,
    observedOffsetMs,
  };
}

function classifyPlatform(host) {
  if (host === 'youtu.be' || host === 'youtube.com' || host.endsWith('.youtube.com')) return 'youtube';
  if (host === 'tiktok.com' || host.endsWith('.tiktok.com')) return 'tiktok';
  if (host === 'instagram.com' || host.endsWith('.instagram.com') || host === 'instagr.am') return 'instagram';
  if (host === 'facebook.com' || host.endsWith('.facebook.com') || host === 'fb.watch' || host === 'fb.com') return 'facebook';
  return null;
}

function extractOffsetMs(u) {
  for (const key of ['t', 'start', 'time_continue']) {
    const val = u.searchParams.get(key);
    if (val) {
      const ms = parseOffsetToken(val);
      if (ms !== null) return ms;
    }
  }
  if (u.hash && u.hash.startsWith('#t=')) {
    return parseOffsetToken(u.hash.slice(3));
  }
  return null;
}

function parseOffsetToken(raw) {
  const s = String(raw).trim().toLowerCase();
  if (/^\d+(\.\d+)?$/.test(s)) {
    const sec = Number(s);
    return sec >= 0 && sec <= 86400 ? Math.round(sec * 1000) : null;
  }
  const m = s.match(/^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$/);
  if (m && (m[1] || m[2] || m[3])) {
    const total = (Number(m[1] || 0) * 3600 + Number(m[2] || 0) * 60 + Number(m[3] || 0)) * 1000;
    return total >= 0 && total <= 86400000 ? total : null;
  }
  return null;
}

function cleanFallbackUrl(u, canonicalHost) {
  const out = new URL(`https://${canonicalHost}${u.pathname || '/'}`);
  for (const [k, v] of u.searchParams.entries()) {
    const lk = k.toLowerCase();
    if (TRACKING_PARAMS.has(lk) || lk.startsWith('utm_') || lk === 't' || lk === 'start' || lk === 'time_continue') continue;
    out.searchParams.append(k, v);
  }
  return out.toString();
}

// --- Best-Effort CORS-Permitted Public oEmbed Enrichment (YouTube & TikTok support browser CORS oEmbed) ---
export async function tryFetchBrowserOEmbed(platform, canonicalUrl) {
  try {
    let endpoint = '';
    if (platform === 'youtube') {
      endpoint = `https://www.youtube.com/oembed?format=json&url=${encodeURIComponent(canonicalUrl)}`;
    } else if (platform === 'tiktok') {
      endpoint = `https://www.tiktok.com/oembed?url=${encodeURIComponent(canonicalUrl)}`;
    } else {
      return null;
    }
    const resp = await fetch(endpoint, { method: 'GET' });
    if (!resp.ok) return null;
    const data = await resp.json();
    return {
      title: data.title || null,
      creatorName: data.author_name || null,
      creatorProfileUrl: data.author_url || null,
      creatorHandle: data.author_unique_id || extractHandleFromUrl(data.author_url),
    };
  } catch {
    return null;
  }
}

function extractHandleFromUrl(urlStr) {
  if (!urlStr || typeof urlStr !== 'string') return '';
  const idx = urlStr.indexOf('/@');
  if (idx >= 0) return urlStr.slice(idx + 2).replace(/\/+$/, '');
  return '';
}

// --- Playback Descriptor Builder (Embeds + Direct R2/Supabase Authorized Video Streams) ---
export function buildPlaybackDescriptor(entry) {
  const directVideoUrl =
    entry.public_video_url ||
    entry.media_assets?.find((a) => a.status === 'ready' && a.public_or_cdn_url)?.public_or_cdn_url ||
    '';

  if (directVideoUrl) {
    return {
      entryId: entry.id,
      platform: entry.platform,
      mode: 'authorized_asset',
      assetStreamUrl: directVideoUrl,
      supportsPlayerTime: true,
      supportsSeek: true,
      precisionNote: 'Playing hosted video asset (Cloudflare R2 / Supabase Storage).',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  const blockedStates = new Set(['private', 'deleted', 'login_required', 'restricted']);
  if (!entry.embed_eligible || blockedStates.has(entry.availability_state)) {
    return {
      entryId: entry.id,
      platform: entry.platform,
      mode: 'fallback_only',
      supportsPlayerTime: false,
      supportsSeek: false,
      fallbackReason: 'Inline playback is restricted or unavailable on the source platform. Open the original post directly.',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  if (entry.platform === 'youtube' && entry.platform_content_id) {
    const isShorts = Boolean(entry.canonical_url?.includes('/shorts/') || entry.original_url?.includes('/shorts/'));
    return {
      entryId: entry.id,
      platform: 'youtube',
      mode: 'iframe_embed',
      orientation: isShorts ? 'portrait' : 'landscape',
      embedUrl: `https://www.youtube-nocookie.com/embed/${encodeURIComponent(entry.platform_content_id)}?enablejsapi=1&rel=0&playsinline=1`,
      allowedOrigins: ['https://www.youtube-nocookie.com', 'https://www.youtube.com'],
      supportsPlayerTime: true,
      supportsSeek: true,
      precisionNote: 'YouTube embedded player (segment seek & time capture supported; approximate to keyframe).',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  if (entry.platform === 'tiktok' && entry.platform_content_id) {
    return {
      entryId: entry.id,
      platform: 'tiktok',
      mode: 'iframe_embed',
      orientation: 'portrait',
      embedUrl: `https://www.tiktok.com/player/v1/${encodeURIComponent(entry.platform_content_id)}?music_info=1&description=1`,
      allowedOrigins: ['https://www.tiktok.com'],
      supportsPlayerTime: true,
      supportsSeek: true,
      precisionNote: 'TikTok Embed Player v1 (segment seek & time capture supported).',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  if (entry.platform === 'facebook') {
    const isReel = Boolean(entry.canonical_url?.includes('/reel/') || entry.original_url?.includes('/reel/'));
    // Facebook's official Reel embed dimension is 267x476 (9:16) so 100% of the iframe stays inside the initial viewport;
    // if the bottom of a Facebook iframe extends below window.innerHeight, Facebook's click-jacking guard blocks the first Play click.
    const fbWidth = isReel ? 267 : 560;
    const fbHeight = isReel ? 476 : 315;
    return {
      entryId: entry.id,
      platform: 'facebook',
      mode: 'iframe_embed',
      orientation: isReel ? 'portrait' : 'landscape',
      embedWidth: fbWidth,
      embedHeight: fbHeight,
      embedUrl: `https://www.facebook.com/plugins/video.php?href=${encodeURIComponent(entry.canonical_url)}&show_text=false&width=${fbWidth}&height=${fbHeight}`,
      allowedOrigins: ['https://www.facebook.com'],
      supportsPlayerTime: false,
      supportsSeek: false,
      precisionNote: 'Facebook Embedded Video Player (timestamps shown below for reference).',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  if (entry.platform === 'instagram' && entry.platform_content_id) {
    const kind = entry.canonical_url?.includes('/p/') ? 'p' : entry.canonical_url?.includes('/tv/') ? 'tv' : 'reel';
    return {
      entryId: entry.id,
      platform: 'instagram',
      mode: 'iframe_embed',
      orientation: 'portrait',
      embedUrl: `https://www.instagram.com/${kind}/${encodeURIComponent(entry.platform_content_id)}/embed/`,
      allowedOrigins: ['https://www.instagram.com'],
      supportsPlayerTime: false,
      supportsSeek: false,
      precisionNote: 'Instagram embed (inline playback depends on creator embed settings; manual timestamps shown below).',
      openOriginalUrl: entry.original_url || entry.canonical_url,
    };
  }

  return {
    entryId: entry.id,
    platform: entry.platform,
    mode: 'fallback_only',
    supportsPlayerTime: false,
    supportsSeek: false,
    fallbackReason: 'Direct embed unavailable for this link format; open the original post to watch.',
    openOriginalUrl: entry.original_url || entry.canonical_url,
  };
}
