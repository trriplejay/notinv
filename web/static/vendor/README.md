# Vendored browser libraries

These are unmodified release bundles downloaded at development time. The page
loads these files from its own origin; no runtime CDN is used.

- `chart.umd.min.js`: Chart.js 4.5.1, `chart.js@4.5.1/dist/chart.umd.min.js` on jsDelivr.
- `chartjs-adapter-date-fns.bundle.min.js`: adapter 3.0.0, `chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js` on jsDelivr. Includes date-fns.

MIT licenses for Chart.js, the adapter, and date-fns are included alongside the
bundles. To upgrade, fetch a pinned release, retain its licenses, and run the
browser tests in `web/tests` to check Chart.js/adapter compatibility.
