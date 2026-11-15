// AI Extraction Prompt Builder & Structured JSON Response Parser for Vueltario Admin Studio

import { normalizeSearchKey, parseTimestampMs, validateBounds } from './utils.js';

export function buildAIExtractionPrompt(entry, moves, extraContext = '') {
  const taxonomyLines = (moves || []).map((m) => {
    const aliases = (m.aliases || []).map((a) => a.alias).filter(Boolean);
    return aliases.length > 0 ? `- ${m.canonical_name} (aliases: ${aliases.join(', ')})` : `- ${m.canonical_name}`;
  });

  const contextSection = extraContext.trim()
    ? `\nAdditional Pasted Caption / Comments / Notes from User:\n"""\n${extraContext.trim()}\n"""\n`
    : '\n(Tip: If you also attached screenshot(s) or video frames of the player, read any on-screen text overlays, creator header, and watermarks from the image(s).)\n';

  return `You are assisting Vueltario, a curated catalog of Cuban dance moves (casino, Timba, and rueda de casino).
Extract accurate video metadata and all demonstrated dance moves (vueltas) for the following social video source.

Source URL: ${entry.original_url || entry.canonical_url}
Canonical URL: ${entry.canonical_url}
Platform: ${entry.platform}
${entry.title ? `Current Title: ${entry.title}\n` : ''}${contextSection}
Existing Vueltario Canonical Moves & Aliases (prefer these exact canonical Spanish names whenever a move or alias matches — for example, map "70" -> "Setenta", "Sácala" -> "Exhibela", "Adiós" -> "Prima"):
${taxonomyLines.join('\n') || '- (none yet)'}

Extraction Rules:
1. Identify the creator's display name, @handle, and profile URL from visible headers, watermarks, or captions. Set "creator_role" to "original" only if clearly the original dancers/choreographers, otherwise "uploader". Leave unknown fields as null (do NOT invent handles).
2. Extract every vuelta / move name from on-screen text overlays, spoken callouts, captions, or comments in the order they appear.
3. If an extracted move matches a canonical move or alias above, output the exact canonical_name and set "is_new_move": false. If it is a new vuelta or named combination not in the list above, keep its clean Spanish name and set "is_new_move": true.
4. Include "start" and "end" timestamps ("MM:SS" or "MM:SS.mmm") ONLY when actually observed or stated in the video/text; otherwise set them to null (never guess timestamps).
5. Reply ONLY with valid raw JSON matching this exact schema (no extra commentary):

{
  "title": "Descriptive Spanish or English video title or null",
  "creator_name": "Creator Display Name or null",
  "creator_handle": "handle_without_at or null",
  "creator_profile_url": "https://... or null",
  "creator_role": "uploader",
  "duration": "MM:SS or null",
  "public_comment": "Brief helpful summary of the lesson/social clip or null",
  "private_notes": "Any extra practice cues/uncertainties for the admin or null",
  "occurrences": [
    {
      "move_name": "Canonical or New Spanish Move Name",
      "is_new_move": false,
      "start": "00:12",
      "end": "00:22",
      "notes": "Optional cue (e.g. from on-screen text overlay)"
    }
  ]
}`;
}

export function parseAIExtractionJSON(rawText, moves = []) {
  const trimmed = String(rawText || '').trim();
  if (!trimmed) {
    throw new Error('Paste the JSON response from ChatGPT, Claude, or your local AI agent first.');
  }

  // Strip optional ```json ... ``` markdown code fences if the LLM wrapped its output
  let candidate = trimmed;
  const fenceMatch = trimmed.match(/```(?:json)?\s*([\s\S]*?)```/i);
  if (fenceMatch) {
    candidate = fenceMatch[1].trim();
  } else {
    const firstBrace = trimmed.indexOf('{');
    const lastBrace = trimmed.lastIndexOf('}');
    if (firstBrace >= 0 && lastBrace > firstBrace) {
      candidate = trimmed.slice(firstBrace, lastBrace + 1);
    }
  }

  let parsed;
  try {
    parsed = JSON.parse(candidate);
  } catch (err) {
    throw new Error(`Invalid JSON: ${err.message}`);
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error('Expected a JSON object matching the Vueltario extraction schema.');
  }

  const durationMs = parsed.duration ? parseTimestampMs(String(parsed.duration)) : null;

  const normalizedOccurrences = [];
  for (const rawOcc of parsed.occurrences || []) {
    const rawName = String(rawOcc?.move_name || '').trim();
    if (!rawName) continue;

    const norm = normalizeSearchKey(rawName);
    const matchedMove = (moves || []).find(
      (m) => m.normalized_search_key === norm || (m.aliases || []).some((a) => a.normalized_alias === norm)
    );

    const startMs = rawOcc.start ? parseTimestampMs(String(rawOcc.start)) : null;
    const endMs = rawOcc.end ? parseTimestampMs(String(rawOcc.end)) : null;
    validateBounds(startMs, endMs, durationMs);

    normalizedOccurrences.push({
      moveId: matchedMove?.id || null,
      moveName: matchedMove ? matchedMove.canonical_name : rawName,
      isNewMove: !matchedMove,
      startMs,
      endMs,
      notes: String(rawOcc.notes || '').trim(),
    });
  }

  const role = parsed.creator_role === 'original' || parsed.creator_role === 'collaborator' ? parsed.creator_role : 'uploader';

  return {
    title: cleanNullableStr(parsed.title),
    creatorName: cleanNullableStr(parsed.creator_name),
    creatorHandle: cleanNullableStr(parsed.creator_handle)?.replace(/^@/, '') || null,
    creatorProfileUrl: cleanNullableStr(parsed.creator_profile_url),
    creatorRole: role,
    durationMs,
    publicComment: cleanNullableStr(parsed.public_comment),
    privateNotes: cleanNullableStr(parsed.private_notes),
    occurrences: normalizedOccurrences,
  };
}

function cleanNullableStr(val) {
  if (val === null || val === undefined) return null;
  const s = String(val).trim();
  return s ? s : null;
}
