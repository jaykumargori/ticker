// Binary wire format (big-endian). Must match backend/internal/proto.
//   frame:  u16 count | f64 serverTs(ms) | { u16 len | packet }*
//   packet: u32 token | f64 ltp | (FULL only) f64 lastQty, open, high, low, close, volume, ts
//   heartbeat: single byte

export const HEADER_LEN = 10;
export const LTP_LEN = 12;
export const FULL_LEN = 68;

export interface Quote {
  ltp: number;
  lastQty: number;
  open: number;
  high: number;
  low: number;
  close: number;
  volume: number;
  ts: number;
}

export const emptyQuote = (): Quote => ({
  ltp: 0, lastQty: 0, open: 0, high: 0, low: 0, close: 0, volume: 0, ts: 0,
});

/** Decoded frame metadata. `packets` is -1 for heartbeat, -2 for malformed. */
export interface FrameInfo {
  serverTs: number;
  packets: number;
}

/**
 * Walks a frame and calls `onPacket` for every packet without allocating per
 * field. The callback reads directly from the DataView.
 */
export function decodeFrame(
  buf: ArrayBuffer,
  onPacket: (token: number, dv: DataView, off: number, len: number) => void,
): FrameInfo {
  if (buf.byteLength <= 1) return { serverTs: 0, packets: -1 };
  if (buf.byteLength < HEADER_LEN) return { serverTs: 0, packets: -2 };
  const dv = new DataView(buf);
  const n = dv.getUint16(0);
  const serverTs = dv.getFloat64(2);
  let off = HEADER_LEN;
  for (let i = 0; i < n; i++) {
    if (off + 2 > buf.byteLength) return { serverTs, packets: -2 };
    const len = dv.getUint16(off);
    off += 2;
    if (len < LTP_LEN || off + len > buf.byteLength) return { serverTs, packets: -2 };
    onPacket(dv.getUint32(off), dv, off, len);
    off += len;
  }
  return { serverTs, packets: n };
}

/** Merges a packet into q. LTP packets only touch `ltp`. */
export function applyPacket(q: Quote, dv: DataView, off: number, len: number): void {
  q.ltp = dv.getFloat64(off + 4);
  if (len < FULL_LEN) return;
  q.lastQty = dv.getFloat64(off + 12);
  q.open = dv.getFloat64(off + 20);
  q.high = dv.getFloat64(off + 28);
  q.low = dv.getFloat64(off + 36);
  q.close = dv.getFloat64(off + 44);
  q.volume = dv.getFloat64(off + 52);
  q.ts = dv.getFloat64(off + 60);
}
