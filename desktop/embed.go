package desktop

import "embed"

// Assets embeds the Wails desktop HTML/CSS/JS frontend (PRD Section 7 & 11).
//
//go:embed index.html css/* js/*
var Assets embed.FS
