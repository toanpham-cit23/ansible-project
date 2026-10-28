const PALETTE = ['#5794f2', '#73bf69', '#fade2a', '#f2495c', '#b877d9', '#ff9830'];

function tipEl() {
  let t = document.getElementById('tip');
  if (!t) { t = document.createElement('div'); t.id = 'tip'; document.body.appendChild(t); }
  return t;
}

function css(name, fallback) {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return v || fallback;
}

function fmtTime(ts) {
  const s = String(ts);
  return s.length >= 16 ? s.slice(11, 16) : s;
}

function drawChart(canvas, series, labels, opts = {}) {
  canvas._d = { series, labels, opts };
  if (!canvas._hover && canvas._hover !== 0) canvas._hover = null;

  if (!canvas._bound) {
    canvas._bound = true;
    canvas.addEventListener('mousemove', (e) => {
      const d = canvas._d;
      if (!d || !d.labels.length) return;
      const r = canvas.getBoundingClientRect();
      const padL = 34, padR = 8;
      const x = e.clientX - r.left;
      const w = r.width - padL - padR;
      const n = d.labels.length;
      let i = Math.round(((x - padL) / w) * (n - 1));
      i = Math.max(0, Math.min(n - 1, i));
      canvas._hover = i;
      paint(canvas);
      const tip = tipEl();
      const rows = d.series.map((s) => {
        const v = s.values[i];
        return `<div class="t-row"><span><i style="display:inline-block;width:8px;height:3px;border-radius:2px;background:${s.color};margin-right:5px;vertical-align:middle"></i>${s.name}</span><b>${v == null ? 'n/a' : v + (d.opts.unit || '')}</b></div>`;
      }).join('');
      tip.innerHTML = `<div class="t-time">${d.labels[i]}</div>${rows}`;
      tip.style.display = 'block';
      const tw = tip.offsetWidth;
      tip.style.left = Math.min(window.innerWidth - tw - 10, e.clientX + 14) + 'px';
      tip.style.top = Math.max(8, e.clientY - tip.offsetHeight - 12) + 'px';
    });
    canvas.addEventListener('mouseleave', () => {
      canvas._hover = null;
      tipEl().style.display = 'none';
      paint(canvas);
    });
  }
  paint(canvas);
}

function paint(canvas) {
  const d = canvas._d;
  if (!d) return;
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth || 300;
  const h = canvas.clientHeight || 150;
  canvas.width = Math.round(w * dpr);
  canvas.height = Math.round(h * dpr);
  const g = canvas.getContext('2d');
  g.setTransform(dpr, 0, 0, dpr, 0, 0);
  g.clearRect(0, 0, w, h);

  const padL = 34, padR = 8, padT = 8, padB = 18;
  const iw = w - padL - padR;
  const ih = h - padT - padB;
  const line = css('--line', '#23262b');
  const muted = css('--muted', '#8e9297');

  const all = d.series.flatMap((s) => s.values).filter((v) => typeof v === 'number' && isFinite(v));
  if (!all.length) {
    g.fillStyle = muted;
    g.font = '11px sans-serif';
    g.fillText('no data', padL, h / 2);
    return;
  }

  let min = d.opts.min !== undefined ? d.opts.min : Math.min(...all);
  let max = d.opts.max !== undefined ? d.opts.max : Math.max(...all);
  if (min === max) { min -= 1; max += 1; }
  const span = max - min;
  min -= span * 0.08;
  max += span * 0.08;

  g.strokeStyle = line;
  g.lineWidth = 1;
  g.fillStyle = muted;
  g.font = '10px sans-serif';
  for (let i = 0; i <= 3; i++) {
    const y = padT + (ih * i) / 3;
    g.beginPath(); g.moveTo(padL, y); g.lineTo(w - padR, y); g.stroke();
    const val = max - ((max - min) * i) / 3;
    g.fillText(String(Math.round(val)), 2, y + 3);
  }

  const n = d.labels.length;
  const X = (i) => padL + (iw * i) / Math.max(1, n - 1);
  const Y = (v) => padT + ih - ((v - min) / (max - min)) * ih;

  d.series.forEach((s, si) => {
    g.strokeStyle = s.color || PALETTE[si % PALETTE.length];
    g.lineWidth = 1.6;
    g.lineJoin = 'round';
    g.beginPath();
    let started = false;
    s.values.forEach((v, i) => {
      if (typeof v !== 'number' || !isFinite(v)) { started = false; return; }
      if (!started) { g.moveTo(X(i), Y(v)); started = true; }
      else g.lineTo(X(i), Y(v));
    });
    g.stroke();
  });

  g.fillStyle = muted;
  g.font = '10px sans-serif';
  const ticks = Math.min(5, n);
  for (let i = 0; i < ticks; i++) {
    const idx = Math.round((n - 1) * (i / Math.max(1, ticks - 1)));
    const label = fmtTime(d.labels[idx]);
    const tw = g.measureText(label).width;
    const x = Math.min(w - padR - tw, Math.max(padL, X(idx) - tw / 2));
    g.fillText(label, x, h - 5);
  }

  if (canvas._hover != null && n) {
    const i = canvas._hover;
    const x = X(i);
    g.strokeStyle = muted;
    g.globalAlpha = 0.5;
    g.beginPath(); g.moveTo(x, padT); g.lineTo(x, padT + ih); g.stroke();
    g.globalAlpha = 1;
    d.series.forEach((s, si) => {
      const v = s.values[i];
      if (typeof v !== 'number' || !isFinite(v)) return;
      g.beginPath();
      g.arc(x, Y(v), 3.4, 0, Math.PI * 2);
      g.fillStyle = s.color || PALETTE[si % PALETTE.length];
      g.fill();
      g.strokeStyle = css('--panel', '#181b1f');
      g.lineWidth = 1.5;
      g.stroke();
    });
  }
}

function drawGauge(el, pct, label) {
  const v = pct == null ? 0 : Math.max(0, Math.min(100, pct));
  const R = 78, cx = 100, cy = 104;
  const len = Math.PI * R;
  const val = (v / 100) * len;
  const tone = v >= 90 ? 'var(--err)' : v >= 75 ? 'var(--warn)' : 'var(--ok)';

  el.innerHTML = `
    <svg class="gauge" viewBox="0 0 200 128" role="img" aria-label="${label} ${v}%">
      <path class="track" d="M ${cx - R} ${cy} A ${R} ${R} 0 0 1 ${cx + R} ${cy}"/>
      <path class="val"   d="M ${cx - R} ${cy} A ${R} ${R} 0 0 1 ${cx + R} ${cy}"
            style="stroke:${tone}" stroke-dasharray="${val} ${len}"/>
      <text class="num"  x="${cx}" y="${cy - 14}" text-anchor="middle">${v.toFixed(1)}%</text>
      <text class="unit" x="${cx}" y="${cy + 2}" text-anchor="middle">${label}</text>
    </svg>`;
}
