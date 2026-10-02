# Releasing

The plugin is distributed through the [Traefik plugin catalog](https://plugins.traefik.io).
The catalog polls GitHub once a day for public repositories with the `traefik-plugin`
topic, fetches tagged versions through the Go module proxy, and test-loads them with the
`testData` in `.traefik.yml`.

## One-time setup

1. Add the **`traefik-plugin`** topic to the repository (GitHub → repository page →
   ⚙ next to *About* → *Topics*). Without it the catalog never sees the plugin.
2. Keep the repository public.
3. Optional: use `.assets/banner.png` as the repository's social preview too (GitHub →
   *Settings* → *General* → *Social preview*). The catalog already uses it as the
   plugin page's sharing image via `bannerPath`.

## Each release

1. Make sure CI is green on `main` (`test`, `lint` and `catalog-smoke`). `catalog-smoke`
   loads the plugin in a real Traefik from `.traefik.yml`'s `testData`, which is what the
   catalog does.
2. In `CHANGELOG.md`, move the `Unreleased` entries into a new `## [X.Y.Z] - YYYY-MM-DD`
   section (for the first release, replace `- unreleased` with the date) and merge that.
3. Release it, either way:
   - **From the browser:** *Actions* → **Release** → *Run workflow*, keep the branch on
     `main`, enter the version (e.g. `v0.1.0`) and run. The workflow runs the checks, then
     creates the tag on `main`'s latest commit and pushes it.
   - **From a clone:** tag `main` and push the tag:

     ```sh
     git tag -a v0.1.0 -m "v0.1.0"
     git push origin v0.1.0
     ```

   The **Release** workflow re-runs the tests, Yaegi tests and the catalog smoke test, then
   publishes a GitHub release with that version's CHANGELOG section as its notes. It
   refuses to run if `CHANGELOG.md` has no section for the version, if the version isn't
   `vX.Y.Z`, or (when run from the browser) if the tag already exists or the branch isn't
   `main`. Don't use GitHub's *Draft a new release* page: it creates the release itself,
   so the workflow's own release step would then fail.
4. Within about a day the version appears in the catalog. If the catalog can't import it,
   it opens an issue in this repository and stops retrying until the issue is closed.

Users then install it with:

```yaml
experimental:
  plugins:
    cfsync:
      moduleName: github.com/KaHooli/Traefik-plugin-cloudflare-ddns
      version: v0.1.0
```

## Notes

- The module path (`go.mod`), `.traefik.yml`'s `import`, and `moduleName` must all be
  exactly `github.com/KaHooli/Traefik-plugin-cloudflare-ddns`, including case. CI checks
  that the manifest and `go.mod` agree.
- The plugin has no dependencies outside the standard library. If one is ever added, it
  must be vendored (`go mod vendor`) and committed; the catalog doesn't download modules.
- Tags are immutable in the Go module proxy: never move or re-push a published tag. Fix
  forward with a new patch version.
