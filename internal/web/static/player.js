// HLS playback plus progress reporting. iOS Safari plays HLS natively, so
// hls.js is only used where it is actually needed.
(function () {
  const config = JSON.parse(document.getElementById('player-config').textContent);
  const video = document.getElementById('player');
  const status = document.getElementById('player-status');

  function say(message) {
    if (status) status.textContent = message || '';
  }

  function attach() {
    if (video.canPlayType('application/vnd.apple.mpegurl')) {
      // Native HLS (Safari, iOS). Seeking is handled by the platform.
      video.src = config.playlist_url;
      return;
    }
    if (!window.Hls || !Hls.isSupported()) {
      say("Ce navigateur ne sait pas lire ce flux HLS.");
      return;
    }

    const hls = new Hls({
      // Segments are produced on demand, so a slow first response is normal
      // rather than a failure.
      manifestLoadingTimeOut: 20000,
      fragLoadingTimeOut: 40000,
      fragLoadingMaxRetry: 4,
      fragLoadingRetryDelay: 1000,
      maxBufferLength: 60,
      startPosition: config.start_at || -1,
    });

    hls.on(Hls.Events.ERROR, (_, data) => {
      if (!data.fatal) return;
      switch (data.type) {
        case Hls.ErrorTypes.NETWORK_ERROR:
          say('Interruption réseau, reprise en cours…');
          hls.startLoad();
          break;
        case Hls.ErrorTypes.MEDIA_ERROR:
          say('Erreur de décodage, récupération en cours…');
          hls.recoverMediaError();
          break;
        default:
          say('Lecture impossible : ' + (data.details || 'erreur inconnue'));
          hls.destroy();
      }
    });

    hls.on(Hls.Events.MANIFEST_PARSED, () => say(''));
    hls.loadSource(config.playlist_url);
    hls.attachMedia(video);
  }

  function addSubtitles() {
    if (!config.subtitle_url) return;
    const track = document.createElement('track');
    track.kind = 'subtitles';
    track.label = config.subtitle_label || 'Sous-titres';
    track.srclang = (config.subtitle_label || 'fr').slice(0, 2);
    track.src = config.subtitle_url;
    track.default = true;
    video.appendChild(track);
  }

  // ---- progress ----

  let lastSaved = -1;

  function saveProgress(useBeacon) {
    const t = video.currentTime;
    if (!isFinite(t) || t <= 0 || Math.abs(t - lastSaved) < 1) return;
    lastSaved = t;

    const body = JSON.stringify({ position_sec: t });
    if (useBeacon && navigator.sendBeacon) {
      // A PUT cannot be sent via beacon, so fall through to a keepalive fetch.
      fetch(config.progress_url, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body,
        keepalive: true,
      }).catch(() => {});
      return;
    }
    fetch(config.progress_url, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body,
    }).catch(() => {});
  }

  setInterval(() => {
    if (!video.paused) saveProgress(false);
  }, 10000);

  video.addEventListener('pause', () => saveProgress(false));
  video.addEventListener('ended', () => saveProgress(false));
  window.addEventListener('pagehide', () => saveProgress(true));
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') saveProgress(true);
  });

  // Native HLS ignores startPosition, so seek once metadata is known.
  video.addEventListener('loadedmetadata', () => {
    if (config.start_at > 0 && video.currentTime < 1) {
      video.currentTime = config.start_at;
    }
  }, { once: true });

  video.addEventListener('waiting', () => say('Mise en mémoire tampon…'));
  video.addEventListener('playing', () => say(''));

  // ---- keyboard ----

  document.addEventListener('keydown', (e) => {
    if (e.target.matches('input, select, textarea')) return;
    switch (e.key) {
      case ' ':
      case 'k':
        e.preventDefault();
        video.paused ? video.play() : video.pause();
        break;
      case 'ArrowRight':
        e.preventDefault();
        video.currentTime = Math.min(video.currentTime + 10, config.duration);
        break;
      case 'ArrowLeft':
        e.preventDefault();
        video.currentTime = Math.max(video.currentTime - 10, 0);
        break;
      case 'f':
        e.preventDefault();
        if (document.fullscreenElement) document.exitFullscreen();
        else if (video.requestFullscreen) video.requestFullscreen();
        else if (video.webkitEnterFullscreen) video.webkitEnterFullscreen();
        break;
      case 'm':
        e.preventDefault();
        video.muted = !video.muted;
        break;
    }
  });

  addSubtitles();
  attach();
})();
