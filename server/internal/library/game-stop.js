/* Game Stop SDK v1. No management credentials or persistent credential storage. */
(function (root) {
  'use strict';
  class GameStopError extends Error {
    constructor(code, message, status = 0) {
      super(message); this.name = 'GameStopError'; this.code = code; this.status = status;
    }
  }
  const invalid = message => { throw new GameStopError('invalid_argument', message); };
  const boardOK = board => board === 'score' || board === 'time-ms';
  class GameStopSDK {
    #base; #game; #token; #timeout; #speech = new Map();
    constructor({ baseURL = 'https://game-stop.giveme8.com', gameId, playToken = '', timeoutMs = 15000 } = {}) {
      let url;
      try { url = new URL(baseURL); } catch { invalid('Invalid baseURL'); }
      if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.pathname !== '/') invalid('baseURL must be an HTTPS origin');
      if (typeof gameId !== 'string' || !/^[a-z0-9][a-z0-9-]{0,63}$/.test(gameId)) invalid('Invalid gameId');
      if (!Number.isInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 60000) invalid('Invalid timeoutMs');
      this.#base = url.origin; this.#game = gameId; this.#timeout = timeoutMs;
      this.setPlayToken(playToken);
    }
    setPlayToken(token) {
      if (typeof token !== 'string' || (token !== '' && !/^ps_[A-Za-z0-9_-]{43}$/.test(token))) invalid('Expected a scoped play token, not an account token');
      this.#token = token;
    }
    static submissionId() {
      if (!root.crypto || !root.crypto.getRandomValues) throw new GameStopError('unsupported', 'Secure randomness unavailable');
      return Array.from(root.crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('');
    }
    async #request(path, body) {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), this.#timeout);
      const headers = { Accept: 'application/json' };
      if (body) {
        if (!this.#token) { clearTimeout(timer); throw new GameStopError('unauthenticated', 'A play session is required'); }
        headers['Content-Type'] = 'application/json'; headers['X-Play-Token'] = this.#token;
      }
      try {
        const response = await root.fetch(this.#base + '/api/v1/games/' + this.#game + path, {
          method: body ? 'POST' : 'GET', headers, body: body ? JSON.stringify(body) : undefined,
          credentials: 'omit', redirect: 'error', referrerPolicy: 'no-referrer', signal: controller.signal
        });
        const text = await response.text();
        if (text.length > 131072) throw new GameStopError('invalid_response', 'Response too large', response.status);
        let data;
        try { data = JSON.parse(text); } catch { throw new GameStopError('invalid_response', 'Expected JSON response', response.status); }
        if (!data || typeof data !== 'object' || Array.isArray(data)) throw new GameStopError('invalid_response', 'Expected JSON object', response.status);
        if (!response.ok) throw new GameStopError('http_error', data.error || 'Request failed', response.status);
        return data;
      } catch (error) {
        if (error instanceof GameStopError) throw error;
        throw new GameStopError(controller.signal.aborted ? 'timeout' : 'network_error', controller.signal.aborted ? 'Request timed out; retry with the same submissionId' : 'Network request failed');
      } finally { clearTimeout(timer); }
    }
    async submitScore(value, { board = 'score', mode = 'default', submissionId = GameStopSDK.submissionId() } = {}) {
      if (typeof mode !== 'string' || !/^[a-z0-9][a-z0-9-]{0,63}$/.test(mode)) invalid('Invalid mode');
      if (!boardOK(board) || !Number.isSafeInteger(value) || value < (board === 'time-ms' ? 1 : 0)) invalid('Invalid board or score value');
      if (typeof submissionId !== 'string' || !/^[A-Za-z0-9_-]{16,64}$/.test(submissionId)) invalid('Invalid submissionId');
      return this.#request('/scores', { board, mode, value, submissionId });
    }
    async getLeaderboard({ board = 'score', mode = 'default', limit = 20 } = {}) {
      if (typeof mode !== 'string' || !/^[a-z0-9][a-z0-9-]{0,63}$/.test(mode)) invalid('Invalid mode');
      if (!boardOK(board) || !Number.isInteger(limit) || limit < 1 || limit > 100) invalid('Invalid leaderboard parameters');
      return this.#request('/leaderboard?board=' + board + '&mode=' + mode + '&limit=' + limit);
    }
    speak(text, { language = 'zh-CN', rate = 1, pitch = 1, volume = 1, interrupt = true } = {}) {
      if (typeof text !== 'string' || !text.trim() || text.length > 2000 || typeof language !== 'string' || language.length > 35 || !Number.isFinite(rate) || rate < 0.1 || rate > 10 || !Number.isFinite(pitch) || pitch < 0 || pitch > 2 || !Number.isFinite(volume) || volume < 0 || volume > 1) return Promise.reject(new GameStopError('invalid_argument', 'Invalid speech arguments'));
      if (!root.speechSynthesis || !root.SpeechSynthesisUtterance) return Promise.reject(new GameStopError('unsupported', 'Text to speech unavailable'));
      if (interrupt) this.stopSpeaking();
      return new Promise((resolve, reject) => {
        const utterance = new root.SpeechSynthesisUtterance(text);
        utterance.lang = language; utterance.rate = rate; utterance.pitch = pitch; utterance.volume = volume;
        const finish = error => {
          const entry = this.#speech.get(utterance);
          if (!entry) return;
          clearTimeout(entry.timer); this.#speech.delete(utterance);
          utterance.onend = null; utterance.onerror = null;
          if (error) reject(error); else resolve();
        };
        const timer = setTimeout(() => { finish(new GameStopError('timeout', 'Speech timed out')); this.stopSpeaking(); }, 120000);
        this.#speech.set(utterance, { timer, finish });
        utterance.onend = () => finish();
        utterance.onerror = event => finish(new GameStopError('speech_error', event.error || 'Speech failed'));
        try { root.speechSynthesis.speak(utterance); } catch { finish(new GameStopError('speech_error', 'Speech failed')); }
      });
    }
    stopSpeaking() {
      for (const entry of [...this.#speech.values()]) entry.finish(new GameStopError('cancelled', 'Speech cancelled'));
      if (root.speechSynthesis) root.speechSynthesis.cancel();
    }
    dispose() { this.stopSpeaking(); this.#token = ''; }
  }
  root.GameStopSDK = GameStopSDK;
  root.GameStopError = GameStopError;
  if (typeof module !== 'undefined' && module.exports) module.exports = { GameStopSDK, GameStopError };
})(globalThis);
