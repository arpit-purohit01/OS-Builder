declare module "@novnc/novnc/core/rfb.js" {
  export default class RFB extends EventTarget {
    constructor(target: HTMLElement, url: string, options?: { wsProtocols?: string[]; shared?: boolean });
    scaleViewport: boolean;
    resizeSession: boolean;
    viewOnly: boolean;
    focusOnClick: boolean;
    disconnect(): void;
    sendCtrlAltDel(): void;
    focus(): void;
  }
}
