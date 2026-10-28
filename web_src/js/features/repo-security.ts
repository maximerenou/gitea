import {registerGlobalInitFunc} from '../modules/observer.ts';

// the Security tab reloads when a scan of the repository ends, once the user is not typing or in a dialog
function initSecurityLive(el: HTMLElement) {
  const busy = () => document.querySelector('.ui.modal.visible') || document.activeElement?.matches('input, textarea, select');
  const reload = () => {
    if (busy()) {
      setTimeout(reload, 5000);
      return;
    }
    window.location.reload();
  };
  const connect = () => {
    const socket = new WebSocket(new URL(el.getAttribute('data-live-url')!, window.location.href).href.replace(/^http/, 'ws'));
    socket.addEventListener('message', reload);
    socket.addEventListener('close', () => setTimeout(connect, 10000));
  };
  connect();
}

export function initRepositorySecurity() {
  registerGlobalInitFunc('initSecurityLive', initSecurityLive);
}
