self.onmessage = ({data: p}) => {
  const bytes = new ArrayBuffer(2 * p.w * p.h);
  const result = new DataView(bytes);
  for (let row = 0; row < p.h; row++) {
    for (let col = 0; col < p.w; col++) {
      const x = p.xmin + (p.x + col + 0.5) * (p.xmax - p.xmin) / p.width;
      const y = p.ymax - (p.y + row + 0.5) * (p.ymax - p.ymin) / p.height;
      let zr = 0, zi = 0, iter = 0;
      while (iter < p.max_iter) {
        const r2 = zr * zr, i2 = zi * zi;
        if (!(r2 + i2 <= 4)) break;
        zi = (2 * zr) * zi + y;
        zr = r2 - i2 + x;
        iter++;
      }
      result.setUint16(2 * (row * p.w + col), iter, true);
    }
  }
  self.postMessage(bytes, [bytes]);
};
