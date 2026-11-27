/* Device classification, loaded synchronously before first paint so the
   phone layout never flashes a desktop-width page. Extracted from an inline
   <script> so the Content-Security-Policy can forbid inline scripts outright.
   It must stay a blocking (non-defer) script: deferring it would let the
   desktop shell paint first on a phone. */
(function () {
  var coarse = false;
  try {
    coarse = window.matchMedia('(pointer: coarse)').matches ||
             window.matchMedia('(hover: none)').matches;
  } catch (e) {
    coarse = ('ontouchstart' in window) || navigator.maxTouchPoints > 0;
  }
  var wide = window.innerWidth > 820;
  var phone = coarse && !wide;
  if (wide) phone = false;      // a wide viewport always gets the desktop shell
  document.documentElement.classList.add(phone ? 'device-phone' : 'device-pc');
  document.documentElement.dataset.input = phone ? 'touch' : 'pointer';
  window.__labIsPhone = function () { return window.innerWidth <= 820; };
  /* Rotating a phone or dragging a desktop window across the 820 px line must
     re-classify, otherwise the page keeps a layout that no longer matches the
     viewport it is in. */
  var last = phone;
  window.addEventListener('resize', function () {
    var now = window.innerWidth <= 820;
    if (now === last) return;
    last = now;
    var root = document.documentElement;
    root.classList.toggle('device-phone', now);
    root.classList.toggle('device-pc', !now);
  }, { passive: true });
})();
