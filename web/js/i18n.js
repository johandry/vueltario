// English default strings with Spanish-ready localization dictionary hooks.
// Cuban dance move names (vueltas) remain in Spanish across all UI locales per PRD Section 2.

export const dictionaries = {
  en: {
    appName: 'Vueltario',
    tagline: 'Cuban Dance Move Catalog — Casino, Timba & Rueda',
    skipToMain: 'Skip to main content',
    navMyMoves: 'My Vueltas',
    navCommunity: 'Community Catalog',
    navTaxonomy: 'Move Taxonomy',
    navAdmin: 'Admin & Publishing',
    btnSignIn: 'Sign In',
    btnRegister: 'Join with Invite',
    btnSignOut: 'Sign Out',
    btnExport: 'Export JSON',
    saveUrlLabel: 'Save a dance video link (YouTube, TikTok, Facebook, Instagram)',
    saveUrlPlaceholder: 'https://www.youtube.com/watch?v=... or TikTok / Instagram / Facebook URL',
    btnSaveLink: 'Save Vuelta Link',
    searchPlaceholder: 'Search move, alias, title, or creator…',
    allPlatforms: 'All platforms',
    allMoves: 'All moves',
    allAssets: 'Any media',
    withAuthorizedAsset: 'Has imported file',
    unknownCreator: 'Unknown creator',
    unknownDuration: 'Unknown duration',
    openOriginal: 'Open original post ↗',
    markStart: 'Mark Start [',
    markEnd: 'Mark End ]',
    previewSegment: 'Preview Segment',
    evidenceSummary: 'Evidence & Assisted Suggestions',
    suggestedBadge: 'Suggested',
    announcementNote: 'Announcement timestamp only (verify movement end)',
    privateExcludedNotice: 'Private notes and private imported video files are strictly excluded from public submissions.',
    dashTagNotice: 'Dash-separated labels (e.g. #CubanSocial-Enchufla) are stored and displayed as requested; Instagram does not parse hyphens inside a single clickable hashtag.',
  },
  // Spanish-ready structure for future locale activation
  es: {
    appName: 'Vueltario',
    tagline: 'Catálogo de Vueltas Cubanas — Casino, Timba y Rueda',
    skipToMain: 'Saltar al contenido principal',
    unknownCreator: 'Creador desconocido',
    openOriginal: 'Abrir publicación original ↗',
  },
};

let activeLocale = 'en';

export function setLocale(locale) {
  if (dictionaries[locale]) {
    activeLocale = locale;
  }
}

export function t(key) {
  return dictionaries[activeLocale]?.[key] ?? dictionaries.en[key] ?? key;
}
