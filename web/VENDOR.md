# Dashboard dependencies

The browser assets are checked in and embedded; there is no runtime package manager
or CDN dependency. Versions and upstream distribution paths:

- Chart.js 4.5.1: `https://cdn.jsdelivr.net/npm/chart.js@4.5.1/dist/chart.umd.min.js`
- chartjs-adapter-date-fns 3.0.0 (bundled date-fns): `https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@3.0.0/dist/chartjs-adapter-date-fns.bundle.min.js`
- Bundled dependency notices: date-fns (MIT), @kurkle/color 0.3.2 (MIT).

Full MIT license notices are included in `static/vendor/LICENSE-*.txt`.
To meet the strict no-URL-literals rule for served assets, upstream documentation
URLs in comments and diagnostic strings were replaced with host/path or descriptive
text. Copyright holder URLs in license notices retain their host/path without the
scheme. Copyright and permission notices remain intact. Source-map trailer comments
were removed because source maps are not shipped. No executable charting logic was
changed.

When updating, download the pinned UMD distributions, retain the license notices,
repeat these notice-only edits, and run the dashboard browser tests as well as the
Go tests. `go test ./...` checks that served files contain no external URL literals.
