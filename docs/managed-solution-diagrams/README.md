# Managed Global VPC engineering diagrams

A Traditional Chinese briefing for platform/network engineers and technical
leadership. Technical names remain in English. This package describes the
managed `platform.globalvpc.io/v1alpha2` implementation and generic validation guidance,
not the historical D/OVN-IC architecture or a production qualification.

- Open [index.html](index.html) for the offline gallery, per-image notes, and
  keyboard-operated presentation mode. Use arrow keys to navigate and Escape
  to exit the presentation. No CDN, web service or build step is needed to view it.
- Read [speaker notes](speaker-notes.zh-TW.md) for the 35–45 minute walkthrough.
  The leadership shortcut is images 01, 02, 06, 09 and 12.
- Each image is available directly as a **3840 × 2160 PNG** and an editable
  **1920 × 1080 SVG**. Drop the PNG files into any 16:9 presentation.
- [manifest.json](manifest.json) maps each figure to its implementation and
  evidence sources. The gallery contains the same notes and source links.
- [overview.png](overview.png) is a contact sheet, not the readable full-size edition.
  Its [HTML source](overview.html) can be captured at a 1800-pixel viewport width.

## Reproduction

From the repository root:

```sh
python3 scripts/render-managed-solution-diagrams.py
node scripts/export-presentation-diagrams.cjs /path/to/playwright /path/to/chromium docs/managed-solution-diagrams
```

The SVG generator also writes the gallery, manifest and speaker notes. The
Chromium exporter checks text bounds and text overlap and stores its report in
`artifacts/managed-solution-diagrams/render-qa.json`. Fonts use PingFang TC /
Heiti TC / Noto Sans CJK TC; rerendering on another system may need a CJK font.
Delivered PNGs have already been rendered and visually inspected.

## Scope and evidence

The canonical [quick start](../managed-quickstart.md),
[validation guidance](../managed-validation.md), and
[native integration contract](../../integration/kube-ovn/README.md) take precedence
above an abbreviated figure. All names, addresses and topology illustrations
are synthetic; no deployment inventories or private run evidence are included.

Version 0.1.0 is experimental and requires a source-matched Kube-OVN extension,
platform identity renewal and compute attachment integration. It does not claim
cloud product parity, stock-native support, zero-loss failover, physical HA,
a particular throughput or a supported production site count.
