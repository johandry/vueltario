// Scoped Player Adapter Bridge (PLAY-01, PLAY-02, TIME-01, TIME-03, Section 13 postMessage validation).
// Never injects untrusted HTML; mounts only structured <video> or allowlisted <iframe> elements.

const ALLOWED_EMBED_PREFIXES = [
  'https://www.youtube-nocookie.com/embed/',
  'https://www.youtube.com/embed/',
  'https://www.tiktok.com/player/v1/',
  'https://www.facebook.com/plugins/video.php',
  'https://www.instagram.com/',
];

function isSafeEmbedURL(url) {
  if (typeof url !== 'string') return false;
  return ALLOWED_EMBED_PREFIXES.some((prefix) => url.startsWith(prefix));
}

export class PlayerController {
  constructor(containerEl, onTimeUpdate) {
    this.containerEl = containerEl;
    this.onTimeUpdate = onTimeUpdate;
    this.descriptor = null;
    this.currentTimeMs = null;
    this.videoEl = null;
    this.iframeEl = null;
    this.stopAtMs = null;
    this.boundMessageHandler = this.handlePostMessage.bind(this);
    window.addEventListener('message', this.boundMessageHandler);
  }

  destroy() {
    window.removeEventListener('message', this.boundMessageHandler);
    this.containerEl.replaceChildren();
    this.videoEl = null;
    this.iframeEl = null;
    this.currentTimeMs = null;
    this.stopAtMs = null;
  }

  mount(descriptor) {
    this.containerEl.replaceChildren();
    this.descriptor = descriptor;
    this.currentTimeMs = null;
    this.videoEl = null;
    this.iframeEl = null;
    this.stopAtMs = null;

    if (!descriptor) return;

    if (descriptor.mode === 'authorized_asset' && descriptor.asset_stream_url) {
      const video = document.createElement('video');
      video.controls = true;
      video.playsInline = true;
      video.preload = 'metadata';
      video.className = 'player-video';
      video.src = descriptor.asset_stream_url;
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

    if (descriptor.mode === 'iframe_embed' && isSafeEmbedURL(descriptor.embed_url)) {
      const iframe = document.createElement('iframe');
      let src = descriptor.embed_url;
      if (descriptor.platform === 'youtube') {
        const sep = src.includes('?') ? '&' : '?';
        src = `${src}${sep}origin=${encodeURIComponent(window.location.origin)}`;
      }
      iframe.src = src;
      iframe.className = 'player-iframe';
      iframe.title = `${descriptor.platform} embedded video player`;
      iframe.loading = 'lazy';
      iframe.referrerPolicy = 'strict-origin-when-cross-origin';
      iframe.allow = 'autoplay; encrypted-media; picture-in-picture; fullscreen';
      iframe.addEventListener('load', () => {
        if (descriptor.platform === 'youtube' && iframe.contentWindow) {
          iframe.contentWindow.postMessage(
            JSON.stringify({ event: 'listening', id: descriptor.source_id }),
            'https://www.youtube-nocookie.com'
          );
        }
      });
      this.iframeEl = iframe;
      this.containerEl.appendChild(iframe);
      return;
    }

    // PLAY-02: Explicit fallback card without auto-redirecting
    const box = document.createElement('div');
    box.className = 'player-fallback';
    const msg = document.createElement('p');
    msg.textContent =
      descriptor.fallback_reason ||
      'Inline playback is unavailable for this source. You can still catalog moves and enter timestamps manually.';
    box.appendChild(msg);
    if (descriptor.open_original_url) {
      const link = document.createElement('a');
      link.href = descriptor.open_original_url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.className = 'btn btn-secondary';
      link.textContent = 'Open original post ↗';
      box.appendChild(link);
    }
    this.containerEl.appendChild(box);
  }

  supportsTimeCapture() {
    return Boolean(this.descriptor?.supports_player_time);
  }

  supportsSeek() {
    return Boolean(this.descriptor?.supports_seek);
  }

  getCurrentTimeMs() {
    if (this.videoEl) {
      return Math.round(this.videoEl.currentTime * 1000);
    }
    return this.currentTimeMs;
  }

  previewSegment(startMs, endMs) {
    if (!this.supportsSeek() || startMs === null || startMs === undefined) {
      return false;
    }
    this.stopAtMs = endMs !== null && endMs !== undefined ? Number(endMs) : null;
    const sec = Math.max(0, Number(startMs) / 1000);

    if (this.videoEl) {
      this.videoEl.currentTime = sec;
      this.videoEl.play().catch(() => {});
      return true;
    }

    if (this.iframeEl?.contentWindow && this.descriptor?.platform === 'youtube') {
      const targetOrigin = 'https://www.youtube-nocookie.com';
      this.iframeEl.contentWindow.postMessage(
        JSON.stringify({ event: 'command', func: 'seekTo', args: [sec, true] }),
        targetOrigin
      );
      this.iframeEl.contentWindow.postMessage(
        JSON.stringify({ event: 'command', func: 'playVideo', args: [] }),
        targetOrigin
      );
      return true;
    }

    if (this.iframeEl?.contentWindow && this.descriptor?.platform === 'tiktok') {
      this.iframeEl.contentWindow.postMessage(
        { 'x-tiktok-player': true, type: 'seekTo', value: sec },
        'https://www.tiktok.com'
      );
      this.iframeEl.contentWindow.postMessage(
        { 'x-tiktok-player': true, type: 'play', value: undefined },
        'https://www.tiktok.com'
      );
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
        { 'x-tiktok-player': true, type: 'pause', value: undefined },
        'https://www.tiktok.com'
      );
    }
  }

  handlePostMessage(event) {
    if (!this.descriptor || !this.iframeEl) return;
    const allowed = this.descriptor.allowed_post_message_origins || [];
    if (!allowed.includes(event.origin)) {
      return;
    }
    if (event.source !== this.iframeEl.contentWindow) {
      return;
    }

    // YouTube IFrame API message schema validation
    if (this.descriptor.platform === 'youtube') {
      let data = event.data;
      if (typeof data === 'string') {
        try {
          data = JSON.parse(data);
        } catch {
          return;
        }
      }
      if (data && typeof data === 'object' && data.event === 'infoDelivery' && data.info && typeof data.info.currentTime === 'number') {
        this.currentTimeMs = Math.round(data.info.currentTime * 1000);
        if (this.stopAtMs !== null && this.currentTimeMs >= this.stopAtMs) {
          this.pauseEmbedded();
          this.stopAtMs = null;
        }
        this.onTimeUpdate?.(this.currentTimeMs);
      }
      return;
    }

    // TikTok Embed Player v1 message schema validation [S5]
    if (this.descriptor.platform === 'tiktok') {
      const data = event.data;
      if (data && typeof data === 'object' && data['x-tiktok-player'] === true && data.type === 'onCurrentTime') {
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
