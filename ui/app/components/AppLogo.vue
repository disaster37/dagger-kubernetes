<template>
  <!-- Fleet-of-daggers logo, inlined (not <img src="/logo.svg">) so the
       currentColor fill resolves against the page's text color: an <img>-SVG
       renders in an isolated document where the theme's `color` (and
       `text-primary`) never cascades, leaving the black glyph invisible on the
       forced-dark theme. Size/labeling come from the usage via attribute
       fallthrough (class, aria-hidden). -->
  <svg
    xmlns="http://www.w3.org/2000/svg"
    viewBox="0 0 64 64"
    fill="currentColor"
    role="img"
    aria-label="Dagger Kubernetes"
  >
    <g transform="translate(21 38) rotate(-18)">
      <path d="M0 -18 L3.5 -5 L0 -3 L-3.5 -5 Z" />
      <rect x="-7" y="-5" width="14" height="2.5" rx="1" />
      <rect x="-2.5" y="-2.5" width="5" height="13" rx="1.5" />
    </g>
    <g transform="translate(32 40)">
      <path d="M0 -18 L3.5 -5 L0 -3 L-3.5 -5 Z" />
      <rect x="-7" y="-5" width="14" height="2.5" rx="1" />
      <rect x="-2.5" y="-2.5" width="5" height="13" rx="1.5" />
    </g>
    <g transform="translate(43 38) rotate(18)">
      <path d="M0 -18 L3.5 -5 L0 -3 L-3.5 -5 Z" />
      <rect x="-7" y="-5" width="14" height="2.5" rx="1" />
      <rect x="-2.5" y="-2.5" width="5" height="13" rx="1.5" />
    </g>
  </svg>
</template>
