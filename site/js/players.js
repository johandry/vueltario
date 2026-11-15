// Official Web Player & R2/Supabase Video Controller for GitHub Pages

const ALLOWED_EMBED_PREFIXES = [
  'https://www.youtube-nocookie.com/embed/',
  'https://www.youtube.com/embed/',
  'https://www.tiktok.com/player/v1/',
  'https://www.facebook.com/plugins/video.php',
  'https://www.instagram.com/',
];

export class PlayerBridge {
  constructor(containerEl, onTimeUpdate = null) {
    this.containerEl = containerEl;
    this.onTimeUpdate = onTimeUpdate;
    this.descriptor = null;
    this.currentTimeMs = null;
    this.stopAtMs = null;
    this.videoEl = null;
    this.iframeEl = null;
    this.onMessage = this.handlePostMessage.bind(this);
    window.addEventListener('message', this.onMessage);
  }

  destroy() {
    window.removeEventListener('message', this.onMessage);
    this.containerEl.replaceChildren();
    this.videoEl = null;
    this.iframeEl = null;
  }

  mount(descriptor) {
    this.containerEl.replaceChildren();
    this.descriptor = descriptor;
    this.currentTimeMs = null;
    this.stopAtMs = null;
    this.videoEl = null;
    this.iframeEl = null;
    if (!descriptor) return;

    if (descriptor.mode === 'authorized_asset' && descriptor.assetStreamUrl) {
      const video = document.createElement('video');
      video.controls = true;
      video.playsInline = true;
      video.preload = 'metadata';
      video.className = 'player-video';
      video.src = descriptor.assetStreamUrl;
      video.addEventListener('timeupdate', () => {
        this.currentTimeMs = Math.round(video.currentTime * 1000);
        if (this.stopAtMs !== null && this.currentTimeMs >= this.stopAtMs) {
          video.pause();
          this.stopAtMs = null;
        }
        this.onTimeUpdate?.(this.currentTimeMs);
      });
      this.videoEl = video;
      this.containerEl.appendChild(video);
      return;
    }

    if (
      descriptor.mode === 'iframe_embed' &&
      typeof descriptor.embedUrl === 'string' &&
      ALLOWED_EMBED_PREFIXES.some((p) => descriptor.embedUrl.startsWith(p))
    ) {
      const isPortrait = descriptor.orientation === 'portrait';
      this.containerEl.classList.toggle('player-stage-portrait', isPortrait);

      const iframe = document.createElement('iframe');
      let src = descriptor.embedUrl;
      if (descriptor.platform === 'youtube') {
        src += `&origin=${encodeURIComponent(window.location.origin)}`;
      }
      iframe.src = src;
      iframe.className = isPortrait ? 'player-iframe player-iframe-portrait' : 'player-iframe player-iframe-landscape';
      if (descriptor.embedWidth) iframe.width = String(descriptor.embedWidth);
      if (descriptor.embedHeight) iframe.height = String(descriptor.embedHeight);
      iframe.scrolling = 'no';
      iframe.allowFullscreen = true;
      iframe.title = `${descriptor.platform} dance video player`;
      iframe.loading = 'eager';
      iframe.referrerPolicy = 'strict-origin-when-cross-origin';
      iframe.allow = 'autoplay; clipboard-write; encrypted-media; picture-in-picture; web-share; fullscreen';
      iframe.addEventListener('load', () => {
        if (descriptor.platform === 'youtube' && iframe.contentWindow) {
          iframe.contentWindow.postMessage(
            JSON.stringify({ event: 'listening', id: descriptor.entryId }),
            'https://www.youtube-nocookie.com'
          );
        }
      });
      this.iframeEl = iframe;
      this.containerEl.appendChild(iframe);
      return;
    }

    const box = document.createElement('div');
    box.className = 'player-fallback';
    const p = document.createElement('p');
    p.textContent = descriptor.fallbackReason || 'Inline playback is unavailable.';
    box.appendChild(p);
    if (descriptor.openOriginalUrl) {
      const a = document.createElement('a');
      a.href = descriptor.openOriginalUrl;
      a.target = '_blank';
      a.rel = 'noopener noreferrer';
      a.className = 'btn btn-secondary';
      a.textContent = 'Open original post ↗';
      box.appendChild(a);
    }
    this.containerEl.appendChild(box);
  }

  supportsTimeCapture() {
    return Boolean(this.descriptor?.supportsPlayerTime);
  }

  supportsSeek() {
    return Boolean(this.descriptor?.supportsSeek);
  }

  getCurrentTimeMs() {
    if (this.videoEl) return Math.round(this.videoEl.currentTime * 1000);
    return this.currentTimeMs;
  }

  seekSegment(startMs, endMs = null) {
    if (!this.supportsSeek() || startMs === null || startMs === undefined) return false;
    this.stopAtMs = endMs !== null && endMs !== undefined ? Number(endMs) : null;
    const sec = Math.max(0, Number(startMs) / 1000);

    if (this.videoEl) {
      this.videoEl.currentTime = sec;
      this.videoEl.play().catch(() => {});
      return true;
    }
    if (this.iframeEl?.contentWindow && this.descriptor?.platform === 'youtube') {
      const target = 'https://www.youtube-nocookie.com';
      this.iframeEl.contentWindow.postMessage(JSON.stringify({ event: 'command', func: 'seekTo', args: [sec, true] }), target);
      this.iframeEl.contentWindow.postMessage(JSON.stringify({ event: 'command', func: 'playVideo', args: [] }), target);
      return true;
    }
    if (this.iframeEl?.contentWindow && this.descriptor?.platform === 'tiktok') {
      this.iframeEl.contentWindow.postMessage({ 'x-tiktok-player': true, type: 'seekTo', value: sec }, 'https://www.tiktok.com');
      this.iframeEl.contentWindow.postMessage({ 'x-tiktok-player': true, type: 'play' }, 'https://www.tiktok.com');
      return true;
    }
    return false;
  }

  pauseEmbedded() {
    if (!this.iframeEl?.contentWindow || !this.descriptor) return;
    if (this.descriptor.platform === 'youtube') {
      this.iframeEl.contentWindow.postMessage(
        JSON.stringify({ event: 'command', func: 'pauseVideo', args: [] }),
        'https://www.youtube-nocookie.com'
      );
    } else if (this.descriptor.platform === 'tiktok') {
      this.iframeEl.contentWindow.postMessage(
        { 'x-tiktok-player': true, type: 'pause' },
        'https://www.tiktok.com'
      );
    }
  }

  handlePostMessage(event) {
    if (!this.descriptor || !this.iframeEl) return;
    if (!(this.descriptor.allowedOrigins || []).includes(event.origin)) return;
    if (event.source !== this.iframeEl.contentWindow) return;

    if (this.descriptor.platform === 'youtube') {
      let data = event.data;
      if (typeof data === 'string') {
        try {
          data = JSON.parse(data);
        } catch {
          return;
        }
      }
      if (data?.event === 'infoDelivery' && typeof data?.info?.currentTime === 'number') {
        this.currentTimeMs = Math.round(data.info.currentTime * 1000);
        if (this.stopAtMs !== null && this.currentTimeMs >= this.stopAtMs) {
          this.pauseEmbedded();
          this.stopAtMs = null;
        }
        this.onTimeUpdate?.(this.currentTimeMs);
      }
    } else if (this.descriptor.platform === 'tiktok') {
      const data = event.data;
      if (data?.['x-tiktok-player'] === true && data?.type === 'onCurrentTime') {
        const sec = typeof data.value === 'number' ? data.value : data.value?.currentTime;
        if (typeof sec === 'number' && Number.isFinite(sec) && sec >= 0) {
          this.currentTimeMs = Math.round(sec * 1000);
          if (this.stopAtMs !== null && this.currentTimeMs >= this.stopAtMs) {
            this.pauseEmbedded();
            this.stopAtMs = null;
          }
          this.onTimeUpdate?.(this.currentTimeMs);
        }
      }
    }
  }
}
