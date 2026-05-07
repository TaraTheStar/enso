module github.com/TaraTheStar/enso/docs

go 1.25

// Hugo modules. The theme is declared in hugo.toml's [module.imports]
// and pulled at build time. The `require` line below is filled in by
// `hugo mod get -u` once a real network is available; on a fresh
// checkout, run:
//
//   cd docs && hugo mod get -u github.com/alex-shpak/hugo-book
//
// CI does this automatically (see .github/workflows/docs.yml).

require github.com/alex-shpak/hugo-book v0.0.0-20260423151019-ae912cc38d3f // indirect
