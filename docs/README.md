# enso documentation site

Built with [Hugo](https://gohugo.io/) + the
[hugo-book](https://github.com/alex-shpak/hugo-book) theme. The theme is
pulled at build time via Hugo Modules; no submodule, no vendoring.

## Local development

```bash
cd docs
hugo mod get -u                 # first run only — fetches the theme
hugo server                     # http://localhost:1313
```

## Building the static site

```bash
cd docs
hugo --minify                   # output lands in docs/public/
```

## Deploying

CI publishes the contents of `public/` to the `gh-pages` branch on
every push to `main`. See `.github/workflows/docs.yml`.

## Editing content

All pages live under `content/`. Folder names become URL paths and
appear in the left sidebar in the order set by each page's frontmatter
`weight`. Pages with `bookHidden: true` are excluded from the sidebar
but still build.

To add a new page, create the `.md` file with this frontmatter:

```yaml
---
title: "Page title"
weight: 10
---
```
