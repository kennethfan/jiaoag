// 与服务端的 WebSocket 连接。
//
// 浏览器不能给 WebSocket 设置请求头，所以令牌放在查询串里（服务端 /ws 支持
// Authorization 与 ?token= 两种，见 docs/web.md）。断线后按 0.5s→5s 退避重连，
// 重连时由上层用 lastSeq 做增量订阅。

export class Link {
  constructor({ url, token, onFrame, onState }) {
    this.url = url;
    this.token = token;
    this.onFrame = onFrame;
    this.onState = onState; // ('connecting'|'open'|'closed', detail)
    this.ws = null;
    this.closedByUs = false;
    this.attempt = 0;
    this.retryTimer = null;
  }

  connect() {
    this.closedByUs = false;
    clearTimeout(this.retryTimer);
    this.onState?.('connecting');
    const sep = this.url.includes('?') ? '&' : '?';
    const ws = new WebSocket(`${this.url}${sep}token=${encodeURIComponent(this.token)}`);
    this.ws = ws;

    ws.onopen = () => {
      this.attempt = 0;
      this.onState?.('open');
      this.send('client.hello', { client_version: 'web', protocol_version: 1 });
    };
    ws.onmessage = (ev) => {
      let env;
      try {
        env = JSON.parse(ev.data);
      } catch {
        return;
      }
      this.onFrame?.(env);
    };
    ws.onclose = (ev) => {
      this.ws = null;
      if (this.closedByUs) {
        this.onState?.('closed', '已断开');
        return;
      }
      const delay = Math.min(500 * 2 ** this.attempt, 5000);
      this.attempt += 1;
      this.onState?.('closed', `连接断开，${(delay / 1000).toFixed(1)}s 后重连`);
      this.retryTimer = setTimeout(() => this.connect(), delay);
      void ev;
    };
    ws.onerror = () => {
      // 细节交给 onclose：onerror 之后必然跟一次 close。
    };
  }

  get isOpen() {
    return this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  send(type, payload) {
    if (!this.isOpen) return false;
    const env = { version: 1, type, payload };
    this.ws.send(JSON.stringify(env));
    return true;
  }

  close() {
    this.closedByUs = true;
    clearTimeout(this.retryTimer);
    this.ws?.close(1000, 'bye');
    this.ws = null;
  }
}
