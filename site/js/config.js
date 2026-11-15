// Public client configuration for GitHub Pages (safe for browser exposure: RLS enforces read-only on `/`,
// and requires authenticated admin JWT from Supabase Auth for any writes on `/admin`).
// Set SUPABASE_URL and SUPABASE_ANON_KEY to your Supabase project values (or inject at deploy time via GitHub Actions).

export const VUELTARIO_CONFIG = {
  supabaseUrl: window.__VUELTARIO_SUPABASE_URL__ || localStorage.getItem('vueltario_supabase_url') || '',
  supabaseAnonKey: window.__VUELTARIO_SUPABASE_ANON_KEY__ || localStorage.getItem('vueltario_supabase_anon_key') || '',
  brandPrefix: 'CubanSocial',
  instagramHandle: '@cubansocial.sd',
  instagramUrl: 'https://www.instagram.com/cubansocial.sd/',
  r2PublicBaseUrl: window.__VUELTARIO_R2_PUBLIC_URL__ || '',
};

export function isSupabaseConfigured() {
  return Boolean(VUELTARIO_CONFIG.supabaseUrl && VUELTARIO_CONFIG.supabaseAnonKey);
}

export function saveLocalSupabaseOverride(url, anonKey) {
  localStorage.setItem('vueltario_supabase_url', (url || '').trim());
  localStorage.setItem('vueltario_supabase_anon_key', (anonKey || '').trim());
}
