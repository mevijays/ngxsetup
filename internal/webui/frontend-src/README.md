# Rebuilding the web UI's compiled CSS

`internal/webui/static/vendor/tailwind.css` is a build artifact, not
hand-written — it's a purged, minified compile of `tailwind-input.css`
against the utility classes actually used in `../static/index.html` and
`../static/app.js`. It's committed (embedded via `go:embed`) so the Go build
itself needs no Node/npm toolchain, consistent with the rest of this project.

To regenerate it after changing markup or component classes:

```bash
# One-time: download the standalone Tailwind CLI (no Node required).
curl -sLo /tmp/tailwindcss \
  "https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-$(uname -s | tr A-Z a-z)-$(uname -m | sed 's/x86_64/x64/;s/aarch64/arm64/')"
chmod +x /tmp/tailwindcss

cd internal/webui/frontend-src
/tmp/tailwindcss -i tailwind-input.css -o ../static/vendor/tailwind.css --minify
```

Then rebuild the Go binary as usual — `go:embed` picks up the new file
automatically.

Font Awesome (`../static/vendor/fontawesome.css` + `webfonts/`) and Chart.js
(`../static/vendor/chart.js`) are vendored as-is from their official releases
and don't need rebuilding, only re-downloading on a version bump.
