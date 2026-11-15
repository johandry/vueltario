// Public client configuration for GitHub Pages.
// NOTE ON SECURITY: Supabase's Project URL and `sb_publishable_...` (anon) key are explicitly designed by Supabase
// to live in client-side browser code. Security is enforced server-side by PostgreSQL Row-Level Security (RLS):
//   - Anonymous visitors (`/`): SELECT only on published entries & curated moves; ZERO write permissions; ZERO access to `admin_entry_notes`.
//   - Admin Studio (`/admin`): Requires a valid Supabase Auth email+password JWT whose UID is in `public.admin_users`.

const DEFAULT_SUPABASE_URL = 'https://setfczirxhlexxguzkur.supabase.co';
const DEFAULT_SUPABASE_PUBLISHABLE_KEY = 'sb_publishable_VSZuHPKTpiqqjf9JxWGUSg_EsG8a4lX';

export const VUELTARIO_CONFIG = {
  supabaseUrl: window.__VUELTARIO_SUPABASE_URL__ || DEFAULT_SUPABASE_URL,
  supabaseAnonKey: window.__VUELTARIO_SUPABASE_ANON_KEY__ || DEFAULT_SUPABASE_PUBLISHABLE_KEY,
  brandPrefix: 'CubanSocial',
  instagramHandle: '@cubansocial.sd',
  instagramUrl: 'https://www.instagram.com/cubansocial.sd/',
  r2PublicBaseUrl: window.__VUELTARIO_R2_PUBLIC_URL__ || '',
};

export function isSupabaseConfigured() {
  return Boolean(VUELTARIO_CONFIG.supabaseUrl && VUELTARIO_CONFIG.supabaseAnonKey);
}
