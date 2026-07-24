// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Pure JS incremental SHA-256 implementation.
// Supports update(Uint8Array) / digest() pattern.
// All intermediate results use >>> 0 to ensure unsigned 32-bit integer operations.

function rot(x, n) { return ((x >>> n) | (x << (32 - n))) >>> 0; }

const Sha256 = (function() {
  const K = [
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5,
    0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3,
    0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc,
    0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
    0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
    0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3,
    0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5,
    0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208,
    0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
  ];

  function Sha256() {
    this.h = [
      0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
      0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
    ];
    this._buf = [];
    this._len = 0;
  }

  Sha256.prototype.update = function(data) {
    if (!data || !data.length) return this;
    this._len += data.length * 8;
    for (let i = 0; i < data.length; i++) {
      this._buf[this._buf.length] = data[i];
      if (this._buf.length === 64) {
        this._transform(this._buf);
        this._buf = [];
      }
    }
    return this;
  };

  Sha256.prototype.digest = function() {
    const lo = (this._len >>> 0) & 0xFFFFFFFF;
    const hi = Math.floor(this._len / 0x100000000) & 0xFFFFFFFF;

    let buf = this._buf.slice();
    buf.push(0x80);
    while (buf.length % 64 !== 56) { buf.push(0x00); }
    buf.push((hi >>> 24) & 0xff);
    buf.push((hi >>> 16) & 0xff);
    buf.push((hi >>> 8) & 0xff);
    buf.push(hi & 0xff);
    buf.push((lo >>> 24) & 0xff);
    buf.push((lo >>> 16) & 0xff);
    buf.push((lo >>> 8) & 0xff);
    buf.push(lo & 0xff);

    for (let di = 0; di < buf.length; di += 64) {
      this._transform(buf.slice(di, di + 64));
    }
    let hex = '';
    for (let i = 0; i < 8; i++) {
      hex += ((this.h[i] >>> 24) & 0xff).toString(16).padStart(2, '0');
      hex += ((this.h[i] >>> 16) & 0xff).toString(16).padStart(2, '0');
      hex += ((this.h[i] >>> 8) & 0xff).toString(16).padStart(2, '0');
      hex += (this.h[i] & 0xff).toString(16).padStart(2, '0');
    }
    return hex;
  };

  Sha256.prototype._transform = function(m) {
    const w = new Array(64);
    for (let ti = 0; ti < 16; ti++) {
      w[ti] = ((m[ti * 4] << 24) | (m[ti * 4 + 1] << 16) | (m[ti * 4 + 2] << 8) | m[ti * 4 + 3]) >>> 0;
    }
    for (let ti = 16; ti < 64; ti++) {
      const s0 = rot(w[ti - 15], 7) ^ rot(w[ti - 15], 18) ^ (w[ti - 15] >>> 3);
      const s1 = rot(w[ti - 2], 17) ^ rot(w[ti - 2], 19) ^ (w[ti - 2] >>> 10);
      w[ti] = ((w[ti - 16] + s0 + w[ti - 7] + s1) & 0xFFFFFFFF) >>> 0;
    }
    let a = this.h[0], b = this.h[1], c = this.h[2], d = this.h[3];
    let e = this.h[4], f = this.h[5], g = this.h[6], hh = this.h[7];
    for (let ti = 0; ti < 64; ti++) {
      const S1 = rot(e, 6) ^ rot(e, 11) ^ rot(e, 25);
      const ch = ((e & f) ^ ((~e) & g)) >>> 0;
      const t1 = ((hh + S1 + ch + K[ti] + w[ti]) & 0xFFFFFFFF) >>> 0;
      const S0 = rot(a, 2) ^ rot(a, 13) ^ rot(a, 22);
      const maj = ((a & b) ^ (a & c) ^ (b & c)) >>> 0;
      const t2 = ((S0 + maj) & 0xFFFFFFFF) >>> 0;
      hh = g; g = f; f = e;
      e = ((d + t1) & 0xFFFFFFFF) >>> 0;
      d = c; c = b; b = a;
      a = ((t1 + t2) & 0xFFFFFFFF) >>> 0;
    }
    this.h[0] = ((this.h[0] + a) & 0xFFFFFFFF) >>> 0;
    this.h[1] = ((this.h[1] + b) & 0xFFFFFFFF) >>> 0;
    this.h[2] = ((this.h[2] + c) & 0xFFFFFFFF) >>> 0;
    this.h[3] = ((this.h[3] + d) & 0xFFFFFFFF) >>> 0;
    this.h[4] = ((this.h[4] + e) & 0xFFFFFFFF) >>> 0;
    this.h[5] = ((this.h[5] + f) & 0xFFFFFFFF) >>> 0;
    this.h[6] = ((this.h[6] + g) & 0xFFFFFFFF) >>> 0;
    this.h[7] = ((this.h[7] + hh) & 0xFFFFFFFF) >>> 0;
  };

  return Sha256;
})();