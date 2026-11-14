package web

import "embed"

// Assets embeds the responsive Vanilla JS web application served from the same origin.
//
//go:embed index.html css/* js/*
var Assets embed.FS
