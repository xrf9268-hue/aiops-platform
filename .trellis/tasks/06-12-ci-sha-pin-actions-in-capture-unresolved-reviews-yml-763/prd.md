# PRD: SHA-pin capture-unresolved-reviews.yml actions (#763)

Pin the three floating action refs (checkout@v6, setup-node@v6,
github-script@v9) to commit SHAs, matching every other workflow in the repo.
SHAs: checkout/setup-node copied from ci.yml; github-script v9 resolved via
git ls-remote (tag v9 peels to 3a2844b7e9c422d3c10d287c895573f7108da1b3 =
v9.0.0).

Acceptance: no floating 'uses:' remain in .github/workflows; fixtures test
green. Refs issue #763.
